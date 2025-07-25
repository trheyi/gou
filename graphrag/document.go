package graphrag

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yaoapp/gou/graphrag/converter"
	"github.com/yaoapp/gou/graphrag/types"
	"github.com/yaoapp/gou/graphrag/utils"
)

// StoreDocumentsOptions contains all options for storing documents to vector store
type StoreDocumentsOptions struct {
	Chunks                           []*types.Chunk
	Entities                         []types.Node
	Relationships                    []types.Relationship
	Embeddings                       *types.EmbeddingResults
	EmbeddingIndexesMap              map[*types.Chunk]int
	EntityIndexMap                   map[*types.Node]int
	RelationshipIndexMap             map[*types.Relationship]int
	SourceFile                       string
	ConvertMetadata                  map[string]interface{}
	UserMetadata                     map[string]interface{}
	VectorCollectionName             string
	CollectionID                     string
	DocID                            string // Add document ID for tracking
	EntityDeduplicationResults       map[string]*EntityDeduplicationResult
	RelationshipDeduplicationResults map[string]*RelationshipDeduplicationResult
	OriginalText                     string // Add original text for potential Vector DB storage
}

// AddFile adds a file to a collection
func (g *GraphRag) AddFile(ctx context.Context, file string, options *types.UpsertOptions) (string, error) {

	// Step 1: Prepare the upsert options
	err := g.prepareUpsert(file, options)
	if err != nil {
		return "", fmt.Errorf("failed to prepare the upsert options: %w", err)
	}

	// Step 1.5: Get collection IDs for vector and graph storage
	collectionID := options.CollectionID
	if collectionID == "" {
		collectionID = "default"
	}

	ids, err := utils.GetCollectionIDs(collectionID)
	if err != nil {
		return "", fmt.Errorf("failed to generate collection IDs: %w", err)
	}

	// Step 2: Create document ID (use user provided or auto-generate)
	var docID string
	if options.DocID != "" {
		docID = options.DocID
	} else {
		docID = utils.GenDocIDWithCollectionID(collectionID)
	}

	// Create the callback for the upsert progress
	var chunks []*types.Chunk = []*types.Chunk{}                          // Store the chunks here
	var embeddings *types.EmbeddingResults                                // Store the embeddings here
	var embeddingTexts []string = []string{}                              // Store the embedding texts here
	var embeddingIndexesMap map[*types.Chunk]int = map[*types.Chunk]int{} // Store the embedding indexes here

	var cb = MakeUpsertCallback(docID, &chunks, options.Progress)

	// Step 3: Convert the file to text
	result, err := options.Converter.Convert(ctx, file, cb.Converter)
	if err != nil {
		return "", fmt.Errorf("failed to convert the file: %w", err)
	}

	// Step 3.5: Store original text - Strategy based on Store configuration
	if g.Store != nil {
		// Store configured - store Origin only in Store
		originKey := fmt.Sprintf(StoreKeyOrigin, docID)
		err = g.Store.Set(originKey, result.Text, 0) // No TTL (permanent storage)
		if err != nil {
			g.Logger.Warnf("Failed to store original text to Store: %v", err)
		} else {
			g.Logger.Infof("Stored original text to Store with key: %s", originKey)
		}
	}
	// Note: If Store is not configured, Origin will be stored in Vector DB metadata in Step 7

	// Step 4.1: Chunk the file
	err = options.Chunking.Chunk(ctx, result.Text, options.ChunkingOptions, cb.Chunking)
	if err != nil {
		return "", fmt.Errorf("failed to chunk the file: %w", err)
	}

	// Step 4.2: Store the embedding indexes for chunks
	for _, chunk := range chunks {
		embeddingTexts = append(embeddingTexts, chunk.Text)
		embeddingIndexesMap[chunk] = len(embeddingTexts) - 1
	}

	// Step 4.3: Extract the root level text (Optional)
	allEntities, allRelationships, entityIndexMap, relationshipIndexMap, err := g.extractEntitiesAndRelationships(ctx, chunks, options, cb, &embeddingTexts)
	if err != nil {
		return "", fmt.Errorf("failed to extract entities and relationships: %w", err)
	}

	// Step 5: Embed all texts (chunks + entities + relationships)
	embeddings, err = options.Embedding.EmbedDocuments(ctx, embeddingTexts, cb.Embedding)
	if err != nil {
		return "", fmt.Errorf("failed to embed the documents: %w", err)
	}

	// Step 6: Store entities and relationships to graph store first for deduplication (if available)
	// Important: We need to do this BEFORE storing to vector database to get the actual deduplicated IDs
	var actualEntityIDs []string
	var actualRelationshipIDs []string
	var entityIDMap = make(map[string]string)       // originalID -> actualID
	var relationshipIDMap = make(map[string]string) // originalID -> actualID

	// Initialize deduplication results
	var entityDeduplicationResults map[string]*EntityDeduplicationResult
	var relationshipDeduplicationResults map[string]*RelationshipDeduplicationResult

	if g.Graph != nil && options.Extraction != nil && (len(allEntities) > 0 || len(allRelationships) > 0) {
		// Store entities to graph store and get actual IDs after deduplication
		if len(allEntities) > 0 {
			actualEntityIDs, entityDeduplicationResults, err = g.storeEntitiesToGraphStore(ctx, allEntities, ids.Graph, docID)
			if err != nil {
				return "", fmt.Errorf("failed to store entities to graph store: %w", err)
			}

			// Create mapping from original IDs to actual IDs
			for i, entity := range allEntities {
				if i < len(actualEntityIDs) {
					entityIDMap[entity.ID] = actualEntityIDs[i]
				}
			}
		}

		// Store relationships to graph store and get actual IDs after deduplication
		if len(allRelationships) > 0 {
			actualRelationshipIDs, relationshipDeduplicationResults, err = g.storeRelationshipsToGraphStore(ctx, allRelationships, ids.Graph, docID)
			if err != nil {
				return "", fmt.Errorf("failed to store relationships to graph store: %w", err)
			}

			// Create mapping from original IDs to actual IDs
			for i, relationship := range allRelationships {
				if i < len(actualRelationshipIDs) {
					relationshipIDMap[relationship.ID] = actualRelationshipIDs[i]
				}
			}
		}

		// Update chunks with actual IDs from graph database
		g.updateChunksWithActualIds(chunks, entityIDMap, relationshipIDMap)
	}

	// Step 7: Store all documents to vector store (chunks + entities + relationships)
	// Now the chunks contain the actual deduplicated IDs from the graph database
	storeOptions := &StoreDocumentsOptions{
		Chunks:                           chunks,
		Entities:                         allEntities,
		Relationships:                    allRelationships,
		Embeddings:                       embeddings,
		EmbeddingIndexesMap:              embeddingIndexesMap,
		EntityIndexMap:                   entityIndexMap,
		RelationshipIndexMap:             relationshipIndexMap,
		SourceFile:                       file,
		ConvertMetadata:                  result.Metadata,
		UserMetadata:                     options.Metadata,
		VectorCollectionName:             ids.Vector,
		CollectionID:                     collectionID,
		DocID:                            docID,
		EntityDeduplicationResults:       entityDeduplicationResults,
		RelationshipDeduplicationResults: relationshipDeduplicationResults,
		OriginalText:                     result.Text, // Pass original text for potential Vector DB storage
	}

	err = g.storeAllDocumentsToVectorStore(ctx, storeOptions)
	if err != nil {
		return "", fmt.Errorf("failed to store documents to vector store: %w", err)
	}

	return docID, nil
}

// AddText adds a text to a collection
func (g *GraphRag) AddText(ctx context.Context, text string, options *types.UpsertOptions) (string, error) {
	// Create temporary markdown file
	tempFile, err := os.CreateTemp("", "graphrag_text_*.md")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tempFile.Name()) // Clean up temporary file

	// Write text to temporary file
	_, err = tempFile.WriteString(text)
	if err != nil {
		tempFile.Close()
		return "", fmt.Errorf("failed to write text to temporary file: %w", err)
	}
	tempFile.Close()

	// Use UTF8 converter if no converter is set, otherwise preserve existing
	optsCopy := *options
	if optsCopy.Converter == nil {
		// No converter set - use default UTF8 converter for text content
		optsCopy.Converter = converter.NewUTF8()
	}
	// If converter is already set, preserve it (user's choice)

	// Process using AddFile
	return g.AddFile(ctx, tempFile.Name(), &optsCopy)
}

// AddURL adds a URL to a collection
func (g *GraphRag) AddURL(ctx context.Context, url string, options *types.UpsertOptions) (string, error) {
	// Get CollectionID for docID prefix
	collectionID := options.CollectionID
	if collectionID == "" {
		collectionID = "default"
	}

	// Create document ID if not provided
	docID := options.DocID
	if docID == "" {
		docID = utils.GenDocIDWithCollectionID(collectionID)
	}

	// Use default fetcher if none provided
	fetcher := options.Fetcher
	if fetcher == nil {
		defaultFetcher, err := DetectFetcher(url)
		if err != nil {
			return "", fmt.Errorf("failed to detect fetcher: %w", err)
		}
		fetcher = defaultFetcher
	}

	// Create callback for fetcher progress
	callback := MakeUpsertCallback(docID, nil, options.Progress)

	// Fetch content from URL with callback
	content, mimeType, err := fetcher.Fetch(ctx, url, callback.Fetcher)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL content: %w", err)
	}

	// Determine if content is binary based on MIME type
	var contentBytes []byte
	if g.isBinaryMimeType(mimeType) {
		// Binary content - decode Base64
		contentBytes, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return "", fmt.Errorf("failed to decode Base64 content: %w", err)
		}
	} else {
		// Text content - convert string to bytes
		contentBytes = []byte(content)
	}

	// Create temporary file with appropriate extension
	extension := g.getExtensionFromMimeType(mimeType)
	tempFile, err := os.CreateTemp("", "graphrag_url_*"+extension)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tempFile.Name()) // Clean up temporary file

	// Write content to temporary file
	_, err = tempFile.Write(contentBytes)
	if err != nil {
		tempFile.Close()
		return "", fmt.Errorf("failed to write content to temporary file: %w", err)
	}
	tempFile.Close()

	// Choose converter based on MIME type and existing settings
	optsCopy := *options   // Properly copy the struct
	optsCopy.DocID = docID // Ensure the generated docID is passed to AddFile

	if g.isBinaryMimeType(mimeType) {
		// For binary content, let AddFile auto-detect converter based on extension
		return g.AddFile(ctx, tempFile.Name(), &optsCopy)
	}

	// For text content, use UTF8 converter if no converter is set, otherwise preserve existing
	if optsCopy.Converter == nil {
		// No converter set - use default UTF8 converter for text content
		optsCopy.Converter = converter.NewUTF8()
	}
	// If converter is already set, preserve it (user's choice)

	return g.AddFile(ctx, tempFile.Name(), &optsCopy)
}

// AddStream adds a stream to a collection
func (g *GraphRag) AddStream(ctx context.Context, stream io.ReadSeeker, options *types.UpsertOptions) (string, error) {
	// Detect file type from stream content
	buffer := make([]byte, 512)
	n, err := stream.Read(buffer)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read stream for type detection: %w", err)
	}

	// Reset stream position
	_, err = stream.Seek(0, io.SeekStart)
	if err != nil {
		return "", fmt.Errorf("failed to reset stream position: %w", err)
	}

	// Detect MIME type
	mimeType := http.DetectContentType(buffer[:n])
	extension := g.getExtensionFromMimeType(mimeType)

	// Create temporary file with appropriate extension
	tempFile, err := os.CreateTemp("", "graphrag_stream_*"+extension)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tempFile.Name()) // Clean up temporary file

	// Copy stream content to temporary file
	_, err = io.Copy(tempFile, stream)
	if err != nil {
		tempFile.Close()
		return "", fmt.Errorf("failed to copy stream to temporary file: %w", err)
	}
	tempFile.Close()

	// Process using AddFile (let AddFile auto-detect converter based on extension)
	return g.AddFile(ctx, tempFile.Name(), options)
}

// RemoveDocs removes documents by IDs
func (g *GraphRag) RemoveDocs(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	g.Logger.Infof("Starting to remove %d documents", len(ids))

	// Step 1: Parse GraphName from IDs and group by GraphName
	graphGroups := make(map[string][]string) // graphName -> docIDs
	for _, docID := range ids {
		graphName, _ := utils.ExtractCollectionIDFromDocID(docID)
		if graphName == "" {
			graphName = "default" // Backward compatibility for docIDs without prefix
		}
		graphGroups[graphName] = append(graphGroups[graphName], docID)
	}

	g.Logger.Infof("Grouped documents into %d graphs: %v", len(graphGroups), func() []string {
		var keys []string
		for k := range graphGroups {
			keys = append(keys, k)
		}
		return keys
	}())

	totalDeleted := 0

	// Step 2: Batch delete documents grouped by GraphName
	for graphName, docIDs := range graphGroups {
		deleted, err := g.removeDocsFromGraph(ctx, graphName, docIDs)
		if err != nil {
			g.Logger.Errorf("Failed to remove documents from graph %s: %v", graphName, err)
			// Continue processing other graphs, don't interrupt the entire deletion process
			continue
		}
		totalDeleted += deleted
		g.Logger.Infof("Successfully removed %d documents from graph %s", deleted, graphName)
	}

	g.Logger.Infof("Total documents removed: %d", totalDeleted)
	return totalDeleted, nil
}

// removeDocsFromGraph removes documents from a specific graph
func (g *GraphRag) removeDocsFromGraph(ctx context.Context, graphName string, docIDs []string) (int, error) {
	// Step 3: Parse corresponding Collection/Graph IDs
	collectionIDs, err := utils.GetCollectionIDs(graphName)
	if err != nil {
		return 0, fmt.Errorf("failed to get collection IDs for graph %s: %w", graphName, err)
	}

	deletedCount := 0

	// Step 4: Remove documents from vector database
	if g.Vector != nil {
		err := g.removeDocsFromVectorStore(ctx, collectionIDs.Vector, docIDs)
		if err != nil {
			g.Logger.Warnf("Failed to remove documents from vector store %s: %v", collectionIDs.Vector, err)
		} else {
			g.Logger.Infof("Removed documents from vector store: %s", collectionIDs.Vector)
		}
	}

	// Step 5: Process entities and relationships in graph database
	if g.Graph != nil && g.Graph.IsConnected() {
		err := g.removeDocsFromGraphStore(ctx, collectionIDs.Graph, docIDs)
		if err != nil {
			g.Logger.Warnf("Failed to process entities/relationships in graph store %s: %v", collectionIDs.Graph, err)
		} else {
			g.Logger.Infof("Processed entities/relationships in graph store: %s", collectionIDs.Graph)
		}
	}

	// Step 6: Remove original data from Store
	if g.Store != nil {
		g.removeDocsFromStore(ctx, docIDs)
	}

	deletedCount = len(docIDs)
	return deletedCount, nil
}

// removeDocsFromVectorStore removes documents from vector database
func (g *GraphRag) removeDocsFromVectorStore(ctx context.Context, collectionName string, docIDs []string) error {
	// Check if collection exists
	exists, err := g.Vector.CollectionExists(ctx, collectionName)
	if err != nil {
		return fmt.Errorf("failed to check collection existence: %w", err)
	}
	if !exists {
		g.Logger.Infof("Vector collection %s does not exist, skipping vector deletion", collectionName)
		return nil
	}

	// Delete all related documents (chunks, entities, relationships)
	// 1. Delete chunks (doc_id = docIDs)
	chunksFilter := map[string]interface{}{
		"doc_id": docIDs,
	}

	err = g.Vector.DeleteDocuments(ctx, &types.DeleteDocumentOptions{
		CollectionName: collectionName,
		Filter:         chunksFilter,
	})
	if err != nil {
		return fmt.Errorf("failed to delete chunks: %w", err)
	}

	// 2. Delete entities (document_type = "entity" AND doc_ids contains any docID)
	for _, docID := range docIDs {
		entityFilter := map[string]interface{}{
			"document_type": "entity",
			"doc_ids":       docID, // The specific implementation of this filter depends on the vector database
		}

		err = g.Vector.DeleteDocuments(ctx, &types.DeleteDocumentOptions{
			CollectionName: collectionName,
			Filter:         entityFilter,
		})
		if err != nil {
			g.Logger.Warnf("Failed to delete entities for docID %s: %v", docID, err)
		}
	}

	// 3. Delete relationships (document_type = "relationship" AND doc_ids contains any docID)
	for _, docID := range docIDs {
		relationshipFilter := map[string]interface{}{
			"document_type": "relationship",
			"doc_ids":       docID,
		}

		err = g.Vector.DeleteDocuments(ctx, &types.DeleteDocumentOptions{
			CollectionName: collectionName,
			Filter:         relationshipFilter,
		})
		if err != nil {
			g.Logger.Warnf("Failed to delete relationships for docID %s: %v", docID, err)
		}
	}

	return nil
}

// removeDocsFromGraphStore processes entities and relationships in graph database
func (g *GraphRag) removeDocsFromGraphStore(ctx context.Context, graphName string, docIDs []string) error {
	// Check if graph exists
	exists, err := g.Graph.GraphExists(ctx, graphName)
	if err != nil {
		return fmt.Errorf("failed to check graph existence: %w", err)
	}
	if !exists {
		g.Logger.Infof("Graph %s does not exist, skipping graph processing", graphName)
		return nil
	}

	// Process entities: remove doc_id, delete if doc_ids becomes empty
	err = g.processEntitiesForDeletion(ctx, graphName, docIDs)
	if err != nil {
		return fmt.Errorf("failed to process entities: %w", err)
	}

	// Process relationships: remove doc_id, delete if doc_ids becomes empty
	err = g.processRelationshipsForDeletion(ctx, graphName, docIDs)
	if err != nil {
		return fmt.Errorf("failed to process relationships: %w", err)
	}

	return nil
}

// processEntitiesForDeletion processes entities by removing doc_ids and deleting if empty
func (g *GraphRag) processEntitiesForDeletion(ctx context.Context, graphName string, docIDs []string) error {
	// Query all entities that contain these doc_ids
	for _, docID := range docIDs {
		// Build query options to get entities containing this docID
		queryOpts := &types.GraphQueryOptions{
			GraphName: graphName,
			QueryType: "cypher",
			Query:     "MATCH (n) WHERE $docID IN n.doc_ids RETURN n",
			Parameters: map[string]interface{}{
				"docID": docID,
			},
		}

		result, err := g.Graph.Query(ctx, queryOpts)
		if err != nil {
			g.Logger.Warnf("Failed to query entities for docID %s: %v", docID, err)
			continue
		}

		// Process each entity in the query results
		for _, node := range result.Nodes {
			// Remove current docID from doc_ids
			docIDsInterface, exists := node.Properties["doc_ids"]
			if !exists {
				continue
			}

			var newDocIDs []string
			switch docIDsValue := docIDsInterface.(type) {
			case []interface{}:
				for _, id := range docIDsValue {
					if idStr, ok := id.(string); ok && idStr != docID {
						newDocIDs = append(newDocIDs, idStr)
					}
				}
			case []string:
				for _, id := range docIDsValue {
					if id != docID {
						newDocIDs = append(newDocIDs, id)
					}
				}
			}

			// If doc_ids is empty, delete the entity
			if len(newDocIDs) == 0 {
				err := g.Graph.DeleteNodes(ctx, &types.DeleteNodesOptions{
					GraphName:  graphName,
					IDs:        []string{node.ID},
					DeleteRels: true, // Also delete related relationships
				})
				if err != nil {
					g.Logger.Warnf("Failed to delete entity %s: %v", node.ID, err)
				} else {
					g.Logger.Debugf("Deleted entity %s (no more doc_ids)", node.ID)
				}
			} else {
				// Update entity's doc_ids
				node.Properties["doc_ids"] = newDocIDs
				graphNode := &types.GraphNode{
					ID:         node.ID,
					Labels:     node.Labels,
					Properties: node.Properties,
				}

				_, err := g.Graph.AddNodes(ctx, &types.AddNodesOptions{
					GraphName: graphName,
					Nodes:     []*types.GraphNode{graphNode},
					Upsert:    true, // Update existing node
				})
				if err != nil {
					g.Logger.Warnf("Failed to update entity %s doc_ids: %v", node.ID, err)
				} else {
					g.Logger.Debugf("Updated entity %s doc_ids: %v", node.ID, newDocIDs)
				}
			}
		}
	}

	return nil
}

// processRelationshipsForDeletion processes relationships by removing doc_ids and deleting if empty
func (g *GraphRag) processRelationshipsForDeletion(ctx context.Context, graphName string, docIDs []string) error {
	// Query all relationships that contain these doc_ids
	for _, docID := range docIDs {
		// Build query options to get relationships containing this docID
		queryOpts := &types.GraphQueryOptions{
			GraphName: graphName,
			QueryType: "cypher",
			Query:     "MATCH ()-[r]->() WHERE $docID IN r.doc_ids RETURN r",
			Parameters: map[string]interface{}{
				"docID": docID,
			},
		}

		result, err := g.Graph.Query(ctx, queryOpts)
		if err != nil {
			g.Logger.Warnf("Failed to query relationships for docID %s: %v", docID, err)
			continue
		}

		// Process each relationship in the query results
		for _, rel := range result.Relationships {
			// Remove current docID from doc_ids
			docIDsInterface, exists := rel.Properties["doc_ids"]
			if !exists {
				continue
			}

			var newDocIDs []string
			switch docIDsValue := docIDsInterface.(type) {
			case []interface{}:
				for _, id := range docIDsValue {
					if idStr, ok := id.(string); ok && idStr != docID {
						newDocIDs = append(newDocIDs, idStr)
					}
				}
			case []string:
				for _, id := range docIDsValue {
					if id != docID {
						newDocIDs = append(newDocIDs, id)
					}
				}
			}

			// If doc_ids is empty, delete the relationship
			if len(newDocIDs) == 0 {
				err := g.Graph.DeleteRelationships(ctx, &types.DeleteRelationshipsOptions{
					GraphName: graphName,
					IDs:       []string{rel.ID},
				})
				if err != nil {
					g.Logger.Warnf("Failed to delete relationship %s: %v", rel.ID, err)
				} else {
					g.Logger.Debugf("Deleted relationship %s (no more doc_ids)", rel.ID)
				}
			} else {
				// Update relationship's doc_ids
				rel.Properties["doc_ids"] = newDocIDs
				graphRel := &types.GraphRelationship{
					ID:         rel.ID,
					Type:       rel.Type,
					StartNode:  rel.StartNode,
					EndNode:    rel.EndNode,
					Properties: rel.Properties,
				}

				_, err := g.Graph.AddRelationships(ctx, &types.AddRelationshipsOptions{
					GraphName:     graphName,
					Relationships: []*types.GraphRelationship{graphRel},
					Upsert:        true, // Update existing relationship
				})
				if err != nil {
					g.Logger.Warnf("Failed to update relationship %s doc_ids: %v", rel.ID, err)
				} else {
					g.Logger.Debugf("Updated relationship %s doc_ids: %v", rel.ID, newDocIDs)
				}
			}
		}
	}

	return nil
}

// removeDocsFromStore removes original documents and segment metadata from Store
func (g *GraphRag) removeDocsFromStore(ctx context.Context, docIDs []string) {
	for _, docID := range docIDs {
		// Delete original text
		originKey := fmt.Sprintf(StoreKeyOrigin, docID)
		err := g.Store.Del(originKey)
		if err != nil {
			g.Logger.Warnf("Failed to delete original text for docID %s: %v", docID, err)
		} else {
			g.Logger.Debugf("Deleted original text for docID %s", docID)
		}

		// Delete all segment metadata for this document
		g.removeAllSegmentMetadataFromStore(ctx, docID)
	}
}

// storeAllDocumentsToVectorStore stores chunks, entities, and relationships as documents in the vector store
func (g *GraphRag) storeAllDocumentsToVectorStore(ctx context.Context, opts *StoreDocumentsOptions) error {
	// Calculate total number of documents
	totalDocuments := len(opts.Chunks) + len(opts.Entities) + len(opts.Relationships)
	if totalDocuments == 0 {
		return nil
	}

	// Prepare documents for vector store
	documents := make([]*types.Document, 0, totalDocuments)

	// Add chunk documents
	for _, chunk := range opts.Chunks {
		// Get embedding for this chunk
		embeddingIndex := opts.EmbeddingIndexesMap[chunk]
		var vector []float64
		if embeddingIndex < len(opts.Embeddings.Embeddings) {
			vector = opts.Embeddings.Embeddings[embeddingIndex]
		}

		// Prepare metadata
		metadata := make(map[string]interface{})

		// Add user metadata
		for k, v := range opts.UserMetadata {
			metadata[k] = v
		}

		// Add convert metadata
		for k, v := range opts.ConvertMetadata {
			metadata[k] = v
		}

		// Add chunk specific metadata
		chunkDetails := map[string]interface{}{
			"id":      chunk.ID,
			"type":    string(chunk.Type),
			"depth":   chunk.Depth,
			"index":   chunk.Index,
			"is_leaf": chunk.Leaf,
			"is_root": chunk.Root,
		}
		if chunk.ParentID != "" {
			chunkDetails["parent_id"] = chunk.ParentID
		}
		metadata["chunk_details"] = chunkDetails
		metadata["source_file"] = opts.SourceFile
		metadata["document_type"] = "chunk"
		metadata["collection_id"] = opts.CollectionID
		metadata["doc_id"] = opts.DocID
		metadata["created_at"] = time.Now().Unix()

		// Add default segment metadata (Vote, Score, Weight) to Vector DB metadata
		metadata["vote"] = 0
		metadata["score"] = 0.0
		metadata["weight"] = 0.0

		// If Store is not configured, also store Origin in Vector DB metadata
		if g.Store == nil && opts.OriginalText != "" {
			metadata["origin"] = opts.OriginalText
		}

		// Add position information to chunk_details
		if chunk.TextPos != nil {
			chunkDetails["text_position"] = map[string]interface{}{
				"start_index": chunk.TextPos.StartIndex,
				"end_index":   chunk.TextPos.EndIndex,
				"start_line":  chunk.TextPos.StartLine,
				"end_line":    chunk.TextPos.EndLine,
			}
		}

		if chunk.MediaPos != nil {
			chunkDetails["media_position"] = map[string]interface{}{
				"start_time": chunk.MediaPos.StartTime,
				"end_time":   chunk.MediaPos.EndTime,
				"page":       chunk.MediaPos.Page,
			}
		}

		// Add extraction results to chunk_details if available
		if chunk.Extracted != nil {
			// Collect entity information
			var entityList []map[string]interface{}
			for _, node := range chunk.Extracted.Nodes {
				entityInfo := map[string]interface{}{
					"id":          node.ID,
					"name":        node.Name,
					"type":        node.Type,
					"description": node.Description,
					"confidence":  node.Confidence,
				}
				entityList = append(entityList, entityInfo)
			}

			// Collect relationship information
			var relationshipList []map[string]interface{}
			for _, rel := range chunk.Extracted.Relationships {
				relationshipInfo := map[string]interface{}{
					"id":          rel.ID,
					"type":        rel.Type,
					"start_node":  rel.StartNode,
					"end_node":    rel.EndNode,
					"description": rel.Description,
					"confidence":  rel.Confidence,
					"weight":      rel.Weight,
				}
				relationshipList = append(relationshipList, relationshipInfo)
			}

			chunkDetails["entities"] = entityList
			chunkDetails["relationships"] = relationshipList
			chunkDetails["extraction_model"] = chunk.Extracted.Model
		}

		documents = append(documents, &types.Document{
			ID:          chunk.ID,
			Content:     chunk.Text,
			DenseVector: vector,
			Metadata:    metadata,
		})
	}

	// Add entity documents using deduplication results from graph store (no additional queries!)
	for _, entity := range opts.Entities {
		// Get embedding for this entity
		embeddingIndex := opts.EntityIndexMap[&entity]
		var vector []float64
		if embeddingIndex < len(opts.Embeddings.Embeddings) {
			vector = opts.Embeddings.Embeddings[embeddingIndex]
		}

		// Use deduplication results from graph store to avoid additional queries
		var docIDs []string
		var normalizedID string
		var isUpdate bool

		if opts.EntityDeduplicationResults != nil {
			if dedupeResult, exists := opts.EntityDeduplicationResults[entity.ID]; exists {
				// Use results from graph store deduplication
				docIDs = dedupeResult.DocIDs
				normalizedID = dedupeResult.NormalizedID
				isUpdate = dedupeResult.IsUpdate
			} else {
				// Fallback: create normalized ID and use current doc_id
				normalizedID = entity.Name
				if entity.Type != "" {
					normalizedID += "_" + entity.Type
				}
				docIDs = []string{opts.DocID}
				isUpdate = false
			}
		} else {
			// Fallback: create normalized ID and use current doc_id
			normalizedID = entity.Name
			if entity.Type != "" {
				normalizedID += "_" + entity.Type
			}
			docIDs = []string{opts.DocID}
			isUpdate = false
		}

		entityDocID := "entity:" + normalizedID

		// Prepare metadata
		metadata := make(map[string]interface{})

		// Add/update user metadata
		for k, v := range opts.UserMetadata {
			metadata[k] = v
		}

		// Add/update convert metadata
		for k, v := range opts.ConvertMetadata {
			metadata[k] = v
		}

		// Add/update entity specific metadata
		metadata["entity_details"] = map[string]interface{}{
			"id":          normalizedID,
			"name":        entity.Name,
			"type":        entity.Type,
			"description": entity.Description,
			"confidence":  entity.Confidence,
		}
		metadata["source_file"] = opts.SourceFile
		metadata["document_type"] = "entity"
		metadata["collection_id"] = opts.CollectionID
		metadata["doc_ids"] = docIDs // Use doc_ids from graph store deduplication
		metadata["updated_at"] = time.Now().Unix()

		// Set created_at only for new entities
		if !isUpdate {
			metadata["created_at"] = time.Now().Unix()
		}

		// Create entity content text
		entityContent := entity.Name
		if entity.Type != "" {
			entityContent += " (" + entity.Type + ")"
		}
		if entity.Description != "" {
			entityContent += ": " + entity.Description
		}

		documents = append(documents, &types.Document{
			ID:          entityDocID,
			Content:     entityContent,
			DenseVector: vector,
			Metadata:    metadata,
		})
	}

	// Add relationship documents using deduplication results from graph store (no additional queries!)
	for _, relationship := range opts.Relationships {
		// Get embedding for this relationship
		embeddingIndex := opts.RelationshipIndexMap[&relationship]
		var vector []float64
		if embeddingIndex < len(opts.Embeddings.Embeddings) {
			vector = opts.Embeddings.Embeddings[embeddingIndex]
		}

		// Use deduplication results from graph store to avoid additional queries
		var docIDs []string
		var normalizedID string
		var isUpdate bool

		if opts.RelationshipDeduplicationResults != nil {
			if dedupeResult, exists := opts.RelationshipDeduplicationResults[relationship.ID]; exists {
				// Use results from graph store deduplication
				docIDs = dedupeResult.DocIDs
				normalizedID = dedupeResult.NormalizedID
				isUpdate = dedupeResult.IsUpdate
			} else {
				// Fallback: create normalized ID and use current doc_id
				normalizedID = relationship.StartNode + "_" + relationship.Type + "_" + relationship.EndNode
				docIDs = []string{opts.DocID}
				isUpdate = false
			}
		} else {
			// Fallback: create normalized ID and use current doc_id
			normalizedID = relationship.StartNode + "_" + relationship.Type + "_" + relationship.EndNode
			docIDs = []string{opts.DocID}
			isUpdate = false
		}

		relationshipDocID := "relationship:" + normalizedID

		// Prepare metadata
		metadata := make(map[string]interface{})

		// Add/update user metadata
		for k, v := range opts.UserMetadata {
			metadata[k] = v
		}

		// Add/update convert metadata
		for k, v := range opts.ConvertMetadata {
			metadata[k] = v
		}

		// Add/update relationship specific metadata
		metadata["relationship_details"] = map[string]interface{}{
			"id":          normalizedID,
			"type":        relationship.Type,
			"start_node":  relationship.StartNode,
			"end_node":    relationship.EndNode,
			"description": relationship.Description,
			"confidence":  relationship.Confidence,
			"weight":      relationship.Weight,
		}
		metadata["source_file"] = opts.SourceFile
		metadata["document_type"] = "relationship"
		metadata["collection_id"] = opts.CollectionID
		metadata["doc_ids"] = docIDs // Use doc_ids from graph store deduplication
		metadata["updated_at"] = time.Now().Unix()

		// Set created_at only for new relationships
		if !isUpdate {
			metadata["created_at"] = time.Now().Unix()
		}

		// Create relationship content text
		relationshipContent := relationship.StartNode + " " + relationship.Type + " " + relationship.EndNode
		if relationship.Description != "" {
			relationshipContent += ": " + relationship.Description
		}

		documents = append(documents, &types.Document{
			ID:          relationshipDocID,
			Content:     relationshipContent,
			DenseVector: vector,
			Metadata:    metadata,
		})
	}

	// Add documents to vector store
	addOpts := &types.AddDocumentOptions{
		CollectionName: opts.VectorCollectionName,
		Documents:      documents,
		Upsert:         true,
		BatchSize:      100,
	}

	_, err := g.Vector.AddDocuments(ctx, addOpts)
	return err
}

// EntityDeduplicationResult contains the result of entity deduplication
type EntityDeduplicationResult struct {
	NormalizedID string
	DocIDs       []string
	IsUpdate     bool
}

// storeEntitiesToGraphStore stores entities to the graph store with doc_ids management
func (g *GraphRag) storeEntitiesToGraphStore(ctx context.Context, entities []types.Node, graphName string, docID string) ([]string, map[string]*EntityDeduplicationResult, error) {
	// Step 1: Query existing entities to check for duplicates
	var entityIDs []string
	for _, entity := range entities {
		// Create a normalized entity ID based on name and type for deduplication
		normalizedID := entity.Name
		if entity.Type != "" {
			normalizedID += "_" + entity.Type
		}
		entityIDs = append(entityIDs, normalizedID)
	}

	// Query existing entities
	getOpts := &types.GetNodesOptions{
		GraphName:         graphName,
		IDs:               entityIDs,
		IncludeProperties: true,
		IncludeMetadata:   true,
	}

	existingNodes, err := g.Graph.GetNodes(ctx, getOpts)
	if err != nil {
		// If error (e.g., graph doesn't exist yet), proceed with new entities
		existingNodes = []*types.GraphNode{}
	}

	// Create a map of existing entities
	existingEntitiesMap := make(map[string]*types.GraphNode)
	for _, node := range existingNodes {
		existingEntitiesMap[node.ID] = node
	}

	// Step 2: Process entities (merge doc_ids for existing, create new for others)
	var graphNodes []*types.GraphNode
	var resultIDs []string
	entityResults := make(map[string]*EntityDeduplicationResult)

	for i, entity := range entities {
		normalizedID := entityIDs[i]

		if existingNode, exists := existingEntitiesMap[normalizedID]; exists {
			// Entity exists - merge doc_ids
			docIDs := []string{docID}
			if existingDocIDs, ok := existingNode.Properties["doc_ids"].([]interface{}); ok {
				for _, id := range existingDocIDs {
					if idStr, ok := id.(string); ok && idStr != docID {
						docIDs = append(docIDs, idStr)
					}
				}
			}

			// Store deduplication result
			entityResults[entity.ID] = &EntityDeduplicationResult{
				NormalizedID: normalizedID,
				DocIDs:       docIDs,
				IsUpdate:     true,
			}

			// Update existing entity properties
			properties := existingNode.Properties
			properties["doc_ids"] = docIDs
			properties["confidence"] = entity.Confidence // Update with latest confidence
			properties["updated_at"] = time.Now().Unix()

			updatedNode := &types.GraphNode{
				ID:         normalizedID,
				Labels:     existingNode.Labels,
				Properties: properties,
				Embedding:  entity.EmbeddingVector, // Update with latest embedding
				CreatedAt:  existingNode.CreatedAt,
				UpdatedAt:  time.Now(),
				Version:    existingNode.Version + 1,
			}
			graphNodes = append(graphNodes, updatedNode)
			resultIDs = append(resultIDs, normalizedID)
		} else {
			// New entity - create with initial doc_ids
			docIDs := []string{docID}

			// Store deduplication result
			entityResults[entity.ID] = &EntityDeduplicationResult{
				NormalizedID: normalizedID,
				DocIDs:       docIDs,
				IsUpdate:     false,
			}

			labels := []string{"Entity"}
			if entity.Type != "" {
				labels = append(labels, entity.Type)
			}
			labels = append(labels, entity.Labels...)

			properties := make(map[string]interface{})
			for k, v := range entity.Properties {
				properties[k] = v
			}
			properties["name"] = entity.Name
			properties["type"] = entity.Type
			properties["description"] = entity.Description
			properties["confidence"] = entity.Confidence
			properties["extraction_method"] = string(entity.ExtractionMethod)
			properties["source_documents"] = entity.SourceDocuments
			properties["source_chunks"] = entity.SourceChunks
			properties["status"] = string(entity.Status)
			properties["version"] = entity.Version
			properties["doc_ids"] = docIDs // Initialize with current doc_id
			properties["created_at"] = time.Now().Unix()
			properties["updated_at"] = time.Now().Unix()

			newNode := &types.GraphNode{
				ID:         normalizedID,
				Labels:     labels,
				Properties: properties,
				Embedding:  entity.EmbeddingVector,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Version:    1,
			}
			graphNodes = append(graphNodes, newNode)
			resultIDs = append(resultIDs, normalizedID)
		}
	}

	// Step 3: Store/Update entities
	if len(graphNodes) > 0 {
		opts := &types.AddNodesOptions{
			GraphName: graphName,
			Nodes:     graphNodes,
			Upsert:    true,
			BatchSize: 100,
		}

		_, err := g.Graph.AddNodes(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
	}

	return resultIDs, entityResults, nil
}

// RelationshipDeduplicationResult contains the result of relationship deduplication
type RelationshipDeduplicationResult struct {
	NormalizedID string
	DocIDs       []string
	IsUpdate     bool
}

// storeRelationshipsToGraphStore stores relationships to the graph store with doc_ids management
func (g *GraphRag) storeRelationshipsToGraphStore(ctx context.Context, relationships []types.Relationship, graphName string, docID string) ([]string, map[string]*RelationshipDeduplicationResult, error) {
	// Step 1: Query existing relationships to check for duplicates
	var relationshipIDs []string
	for _, relationship := range relationships {
		// Create a normalized relationship ID based on start_node, type, and end_node for deduplication
		normalizedID := relationship.StartNode + "_" + relationship.Type + "_" + relationship.EndNode
		relationshipIDs = append(relationshipIDs, normalizedID)
	}

	// Query existing relationships
	getOpts := &types.GetRelationshipsOptions{
		GraphName:         graphName,
		IDs:               relationshipIDs,
		IncludeProperties: true,
		IncludeMetadata:   true,
	}

	existingRels, err := g.Graph.GetRelationships(ctx, getOpts)
	if err != nil {
		// If error (e.g., graph doesn't exist yet), proceed with new relationships
		existingRels = []*types.GraphRelationship{}
	}

	// Create a map of existing relationships
	existingRelsMap := make(map[string]*types.GraphRelationship)
	for _, rel := range existingRels {
		existingRelsMap[rel.ID] = rel
	}

	// Step 2: Process relationships (merge doc_ids for existing, create new for others)
	var graphRelationships []*types.GraphRelationship
	var resultIDs []string
	relationshipResults := make(map[string]*RelationshipDeduplicationResult)

	for i, relationship := range relationships {
		normalizedID := relationshipIDs[i]

		if existingRel, exists := existingRelsMap[normalizedID]; exists {
			// Relationship exists - merge doc_ids
			docIDs := []string{docID}
			if existingDocIDs, ok := existingRel.Properties["doc_ids"].([]interface{}); ok {
				for _, id := range existingDocIDs {
					if idStr, ok := id.(string); ok && idStr != docID {
						docIDs = append(docIDs, idStr)
					}
				}
			}

			// Store deduplication result
			relationshipResults[relationship.ID] = &RelationshipDeduplicationResult{
				NormalizedID: normalizedID,
				DocIDs:       docIDs,
				IsUpdate:     true,
			}

			// Update existing relationship properties
			properties := existingRel.Properties
			properties["doc_ids"] = docIDs
			properties["confidence"] = relationship.Confidence // Update with latest confidence
			properties["weight"] = relationship.Weight         // Update with latest weight
			properties["updated_at"] = time.Now().Unix()

			updatedRel := &types.GraphRelationship{
				ID:         normalizedID,
				Type:       existingRel.Type,
				StartNode:  existingRel.StartNode,
				EndNode:    existingRel.EndNode,
				Properties: properties,
				Embedding:  relationship.EmbeddingVector, // Update with latest embedding
				CreatedAt:  existingRel.CreatedAt,
				UpdatedAt:  time.Now(),
				Version:    existingRel.Version + 1,
			}
			graphRelationships = append(graphRelationships, updatedRel)
			resultIDs = append(resultIDs, normalizedID)
		} else {
			// New relationship - create with initial doc_ids
			docIDs := []string{docID}

			// Store deduplication result
			relationshipResults[relationship.ID] = &RelationshipDeduplicationResult{
				NormalizedID: normalizedID,
				DocIDs:       docIDs,
				IsUpdate:     false,
			}

			properties := make(map[string]interface{})
			for k, v := range relationship.Properties {
				properties[k] = v
			}
			properties["description"] = relationship.Description
			properties["confidence"] = relationship.Confidence
			properties["weight"] = relationship.Weight
			properties["extraction_method"] = string(relationship.ExtractionMethod)
			properties["source_documents"] = relationship.SourceDocuments
			properties["source_chunks"] = relationship.SourceChunks
			properties["status"] = string(relationship.Status)
			properties["version"] = relationship.Version
			properties["doc_ids"] = docIDs // Initialize with current doc_id
			properties["created_at"] = time.Now().Unix()
			properties["updated_at"] = time.Now().Unix()

			newRel := &types.GraphRelationship{
				ID:         normalizedID,
				Type:       relationship.Type,
				StartNode:  relationship.StartNode,
				EndNode:    relationship.EndNode,
				Properties: properties,
				Embedding:  relationship.EmbeddingVector,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Version:    1,
			}
			graphRelationships = append(graphRelationships, newRel)
			resultIDs = append(resultIDs, normalizedID)
		}
	}

	// Step 3: Store/Update relationships
	if len(graphRelationships) > 0 {
		opts := &types.AddRelationshipsOptions{
			GraphName:     graphName,
			Relationships: graphRelationships,
			Upsert:        true,
			BatchSize:     100,
		}

		_, err := g.Graph.AddRelationships(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
	}

	return resultIDs, relationshipResults, nil
}

// prepareUpsert prepares the upsert options
func (g *GraphRag) prepareUpsert(file string, options *types.UpsertOptions) error {
	// Step 1:  Use the options.Converter if provided, otherwise detect the converter to use for the file
	if options.Converter == nil {
		converter, err := DetectConverter(file)
		if err != nil {
			return err
		}
		options.Converter = converter
	}

	// Step 2: Use the options.Chunking if provided, otherwise detect the chunking to use for the file
	if options.Chunking == nil {
		chunking, err := DetectChunking(file)
		if err != nil {
			return err
		}
		options.Chunking = chunking
	}

	// Step 2.1: Set ChunkingOptions if not provided
	if options.ChunkingOptions == nil {
		// Auto-detect chunking type based on file extension
		chunkingType := types.GetChunkingTypeFromFilename(file)
		options.ChunkingOptions = &types.ChunkingOptions{
			Type:          chunkingType,
			Size:          300,
			Overlap:       20,
			MaxDepth:      3,
			MaxConcurrent: 10,
		}
	}

	// Step 3: Use the options.Embedding if provided, otherwise detect the embedding to use for the file
	if options.Embedding == nil {
		embedding, err := DetectEmbedding(file)
		if err != nil {
			return err
		}
		options.Embedding = embedding
	}

	// Step 4: Use the options.Extraction if provided, otherwise detect the extraction to use for the file
	if g.Graph != nil && options.Extraction == nil {
		extraction, err := DetectExtractor(file)
		if err != nil {
			return err
		}
		options.Extraction = extraction
	}

	return nil
}

// extractEntitiesAndRelationships extracts entities and relationships from root-level chunks
func (g *GraphRag) extractEntitiesAndRelationships(ctx context.Context, chunks []*types.Chunk, options *types.UpsertOptions, cb types.UpsertCallback, embeddingTexts *[]string) ([]types.Node, []types.Relationship, map[*types.Node]int, map[*types.Relationship]int, error) {
	var allEntities []types.Node
	var allRelationships []types.Relationship
	var entityIndexMap map[*types.Node]int = make(map[*types.Node]int)
	var relationshipIndexMap map[*types.Relationship]int = make(map[*types.Relationship]int)

	// Only proceed if graph store and extraction are available
	if g.Graph == nil || options.Extraction == nil {
		return allEntities, allRelationships, entityIndexMap, relationshipIndexMap, nil
	}

	// Find root-level chunks (chunks without parents)
	var extractedChunks []*types.Chunk
	var extractedTexts []string
	for _, chunk := range chunks {
		if len(chunk.Parents) == 0 {
			extractedChunks = append(extractedChunks, chunk)
			extractedTexts = append(extractedTexts, chunk.Text)
		}
	}

	// If no root-level chunks found, return empty results
	if len(extractedTexts) == 0 {
		return allEntities, allRelationships, entityIndexMap, relationshipIndexMap, nil
	}

	// Extract entities and relationships from root-level chunks
	results, err := options.Extraction.ExtractDocuments(ctx, extractedTexts, cb.Extraction)
	if err != nil {
		return allEntities, allRelationships, entityIndexMap, relationshipIndexMap, fmt.Errorf("failed to extract the text from the file: %w", err)
	}

	// Merge the extracted chunks with the original chunks and collect entities/relationships
	for i, result := range results {
		if i < len(extractedChunks) {
			extractedChunks[i].Extracted = result

			// Collect all entities and relationships for embedding
			for j := range result.Nodes {
				entity := &result.Nodes[j]
				allEntities = append(allEntities, *entity)

				// Create embedding text for entity (name + type + description)
				entityText := entity.Name
				if entity.Type != "" {
					entityText += " (" + entity.Type + ")"
				}
				if entity.Description != "" {
					entityText += ": " + entity.Description
				}

				*embeddingTexts = append(*embeddingTexts, entityText)
				entityIndexMap[entity] = len(*embeddingTexts) - 1
			}

			for j := range result.Relationships {
				relationship := &result.Relationships[j]
				allRelationships = append(allRelationships, *relationship)

				// Create embedding text for relationship (StartNode + type + EndNode + description)
				relationshipText := relationship.StartNode + " " + relationship.Type + " " + relationship.EndNode
				if relationship.Description != "" {
					relationshipText += ": " + relationship.Description
				}

				*embeddingTexts = append(*embeddingTexts, relationshipText)
				relationshipIndexMap[relationship] = len(*embeddingTexts) - 1
			}
		}
	}

	return allEntities, allRelationships, entityIndexMap, relationshipIndexMap, nil
}

// updateChunksWithActualIds updates chunks with actual IDs from graph database after deduplication
func (g *GraphRag) updateChunksWithActualIds(chunks []*types.Chunk, entityIDMap, relationshipIDMap map[string]string) {
	for _, chunk := range chunks {
		if chunk.Extracted != nil {
			// Update entity IDs
			for i := range chunk.Extracted.Nodes {
				if actualID, exists := entityIDMap[chunk.Extracted.Nodes[i].ID]; exists {
					chunk.Extracted.Nodes[i].ID = actualID
				}
			}

			// Update relationship IDs and node references
			for i := range chunk.Extracted.Relationships {
				// Update relationship ID
				if actualID, exists := relationshipIDMap[chunk.Extracted.Relationships[i].ID]; exists {
					chunk.Extracted.Relationships[i].ID = actualID
				}

				// Update start node ID
				if actualID, exists := entityIDMap[chunk.Extracted.Relationships[i].StartNode]; exists {
					chunk.Extracted.Relationships[i].StartNode = actualID
				}

				// Update end node ID
				if actualID, exists := entityIDMap[chunk.Extracted.Relationships[i].EndNode]; exists {
					chunk.Extracted.Relationships[i].EndNode = actualID
				}
			}
		}
	}
}

// isBinaryMimeType checks if the MIME type represents binary content
func (g *GraphRag) isBinaryMimeType(mimeType string) bool {
	// Text-based MIME types (should not be Base64 encoded)
	textTypes := []string{
		"text/",
		"application/json",
		"application/xml",
		"application/javascript",
		"application/x-javascript",
		"application/x-httpd-php",
		"application/x-sh",
		"application/x-csh",
		"application/x-perl",
		"application/x-python",
		"application/x-ruby",
		"application/x-tcl",
		"application/xhtml+xml",
		"application/rss+xml",
		"application/atom+xml",
	}

	// Check if it's a text-based MIME type
	for _, textType := range textTypes {
		if strings.HasPrefix(mimeType, textType) {
			return false
		}
	}

	// Binary MIME types (should be Base64 encoded)
	binaryTypes := []string{
		"image/",
		"video/",
		"audio/",
		"application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument",
		"application/vnd.ms-",
		"application/zip",
		"application/x-zip",
		"application/x-rar",
		"application/x-7z",
		"application/x-tar",
		"application/gzip",
		"application/x-gzip",
		"application/octet-stream",
		"application/x-binary",
		"application/x-msdownload",
		"application/x-executable",
		"application/x-deb",
		"application/x-rpm",
		"application/vnd.android.package-archive",
	}

	// Check if it's a binary MIME type
	for _, binaryType := range binaryTypes {
		if strings.HasPrefix(mimeType, binaryType) {
			return true
		}
	}

	// Default: if unknown MIME type, assume it's binary for safety
	// This ensures Base64 content gets properly decoded
	return true
}

// getExtensionFromMimeType returns appropriate file extension for MIME type
func (g *GraphRag) getExtensionFromMimeType(mimeType string) string {
	// Try to get extension from standard mime package
	exts, err := mime.ExtensionsByType(mimeType)
	if err == nil && len(exts) > 0 {
		return exts[0]
	}

	// Fallback to common mappings
	switch {
	case strings.HasPrefix(mimeType, "text/plain"):
		return ".txt"
	case strings.HasPrefix(mimeType, "text/html"):
		return ".html"
	case strings.HasPrefix(mimeType, "text/markdown"):
		return ".md"
	case strings.HasPrefix(mimeType, "application/pdf"):
		return ".pdf"
	case strings.HasPrefix(mimeType, "application/json"):
		return ".json"
	case strings.HasPrefix(mimeType, "text/csv"):
		return ".csv"
	case strings.HasPrefix(mimeType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mimeType, "image/png"):
		return ".png"
	case strings.HasPrefix(mimeType, "image/gif"):
		return ".gif"
	case strings.HasPrefix(mimeType, "image/bmp"):
		return ".bmp"
	case strings.HasPrefix(mimeType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mimeType, "image/tiff"):
		return ".tiff"
	case strings.HasPrefix(mimeType, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mimeType, "video/"):
		return ".mp4" // Default video extension
	case strings.HasPrefix(mimeType, "audio/mpeg"):
		return ".mp3"
	case strings.HasPrefix(mimeType, "audio/wav"):
		return ".wav"
	case strings.HasPrefix(mimeType, "audio/"):
		return ".mp3" // Default audio extension
	case strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"):
		return ".docx"
	case strings.HasPrefix(mimeType, "application/vnd.openxmlformats-officedocument.presentationml.presentation"):
		return ".pptx"
	case strings.HasPrefix(mimeType, "application/msword"):
		return ".doc"
	case strings.HasPrefix(mimeType, "application/vnd.ms-powerpoint"):
		return ".ppt"
	default:
		return ".txt" // Default fallback
	}
}
