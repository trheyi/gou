package rag

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/yaoapp/gou/rag/driver"
)

func getTestConfig(t *testing.T, indexName string) (driver.VectorizeConfig, driver.IndexConfig) {
	// OpenAI config
	openaiKey := os.Getenv("OPENAI_TEST_KEY")
	if openaiKey == "" {
		t.Skip("OPENAI_TEST_KEY not set")
	}

	vectorizeConfig := driver.VectorizeConfig{
		Model: "text-embedding-ada-002",
		Options: map[string]string{
			"api_key": openaiKey,
		},
	}

	// Qdrant config
	host := os.Getenv("QDRANT_TEST_HOST")
	if host == "" {
		host = "localhost"
	}

	port := os.Getenv("QDRANT_TEST_PORT")
	if port == "" {
		port = "6334"
	}

	indexConfig := driver.IndexConfig{
		Name: indexName,
		Options: map[string]string{
			"host":    host,
			"port":    port,
			"api_key": "",
		},
	}

	return vectorizeConfig, indexConfig
}

func TestRAGIntegration(t *testing.T) {
	ctx := context.Background()
	baseIndexName := "test_rag_index"

	// Test file upload
	t.Run("file upload", func(t *testing.T) {
		indexName := fmt.Sprintf("%s_file_%s", baseIndexName, uuid.New().String())
		vectorizeConfig, indexConfig := getTestConfig(t, indexName)

		// Create vectorizer first
		vectorizer, err := NewVectorizer(DriverOpenAI, vectorizeConfig)
		assert.NoError(t, err)
		defer vectorizer.Close()

		// Create engine with vectorizer
		engine, err := NewEngine(DriverQdrant, indexConfig, vectorizer)
		assert.NoError(t, err)
		defer engine.Close()

		// Create file upload
		fileUpload, err := NewFileUpload(DriverQdrant, engine, vectorizer)
		assert.NoError(t, err)

		err = engine.CreateIndex(ctx, indexConfig)
		assert.NoError(t, err)
		defer engine.DeleteIndex(ctx, indexConfig.Name)

		tests := []struct {
			name    string
			content string
			opts    driver.FileUploadOptions
			wantErr bool
		}{
			{
				name:    "Basic upload",
				content: "This is a test document",
				opts: driver.FileUploadOptions{
					IndexName: indexConfig.Name,
					ChunkSize: 100,
				},
				wantErr: false,
			},
			{
				name:    "Async upload",
				content: "This is an async test document",
				opts: driver.FileUploadOptions{
					IndexName: indexConfig.Name,
					ChunkSize: 100,
					Async:     true,
				},
				wantErr: false,
			},
			{
				name:    "Upload with chunks",
				content: strings.Repeat("Test content ", 100),
				opts: driver.FileUploadOptions{
					IndexName:    indexConfig.Name,
					ChunkSize:    100,
					ChunkOverlap: 20,
				},
				wantErr: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				reader := strings.NewReader(tt.content)
				result, err := fileUpload.Upload(ctx, reader, tt.opts)

				if tt.wantErr {
					assert.Error(t, err)
					return
				}

				assert.NoError(t, err)
				assert.NotNil(t, result)
				assert.Greater(t, len(result.Documents), 0)

				if tt.opts.Async {
					assert.NotEmpty(t, result.TaskID)
					// Check task status
					taskInfo, err := engine.GetTaskInfo(ctx, result.TaskID)
					assert.NoError(t, err)
					assert.NotNil(t, taskInfo)
				}
			})
		}

		// Test file upload
		t.Run("file upload", func(t *testing.T) {
			// Create a temporary test file
			content := "This is a test file content\nWith multiple lines\nFor testing purposes"
			tmpfile, err := os.CreateTemp("", "test_upload_*.txt")
			assert.NoError(t, err)
			defer os.Remove(tmpfile.Name())

			_, err = tmpfile.WriteString(content)
			assert.NoError(t, err)
			err = tmpfile.Close()
			assert.NoError(t, err)

			tests := []struct {
				name    string
				opts    driver.FileUploadOptions
				wantErr bool
			}{
				{
					name: "Basic file upload",
					opts: driver.FileUploadOptions{
						IndexName: indexConfig.Name,
						ChunkSize: 100,
					},
					wantErr: false,
				},
				{
					name: "Async file upload",
					opts: driver.FileUploadOptions{
						IndexName: indexConfig.Name,
						ChunkSize: 100,
						Async:     true,
					},
					wantErr: false,
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					result, err := fileUpload.UploadFile(ctx, tmpfile.Name(), tt.opts)

					if tt.wantErr {
						assert.Error(t, err)
						return
					}

					assert.NoError(t, err)
					assert.NotNil(t, result)
					assert.Greater(t, len(result.Documents), 0)

					if tt.opts.Async {
						assert.NotEmpty(t, result.TaskID)
						// Check task status
						taskInfo, err := engine.GetTaskInfo(ctx, result.TaskID)
						assert.NoError(t, err)
						assert.NotNil(t, taskInfo)
					}
				})
			}
		})
	})

	// Test basic vectorization
	t.Run("vectorization", func(t *testing.T) {
		indexName := fmt.Sprintf("%s_vec_%s", baseIndexName, uuid.New().String())
		vectorizeConfig, _ := getTestConfig(t, indexName)

		vectorizer, err := NewVectorizer(DriverOpenAI, vectorizeConfig)
		assert.NoError(t, err)
		defer vectorizer.Close()

		text := "This is a test document for OpenAI embeddings."
		embedding, err := vectorizer.Vectorize(ctx, text)
		assert.NoError(t, err)
		assert.Equal(t, 1536, len(embedding)) // text-embedding-ada-002 produces 1536-dimensional vectors
	})

	// Test batch vectorization
	t.Run("batch vectorization", func(t *testing.T) {
		indexName := fmt.Sprintf("%s_batch_vec_%s", baseIndexName, uuid.New().String())
		vectorizeConfig, _ := getTestConfig(t, indexName)

		vectorizer, err := NewVectorizer(DriverOpenAI, vectorizeConfig)
		assert.NoError(t, err)
		defer vectorizer.Close()

		texts := []string{
			"First test document",
			"Second test document",
			"Third test document",
		}
		embeddings, err := vectorizer.VectorizeBatch(ctx, texts)
		assert.NoError(t, err)
		assert.Equal(t, len(texts), len(embeddings))
		for _, embedding := range embeddings {
			assert.Equal(t, 1536, len(embedding))
		}
	})

	// Test index operations
	t.Run("index operations", func(t *testing.T) {
		indexName := fmt.Sprintf("%s_ops_%s", baseIndexName, uuid.New().String())
		vectorizeConfig, indexConfig := getTestConfig(t, indexName)

		vectorizer, err := NewVectorizer(DriverOpenAI, vectorizeConfig)
		assert.NoError(t, err)
		defer vectorizer.Close()

		engine, err := NewEngine(DriverQdrant, indexConfig, vectorizer)
		assert.NoError(t, err)
		defer engine.Close()

		// Create index
		err = engine.CreateIndex(ctx, indexConfig)
		assert.NoError(t, err)
		defer engine.DeleteIndex(ctx, indexConfig.Name)

		// List indexes
		indexes, err := engine.ListIndexes(ctx)
		assert.NoError(t, err)
		assert.Contains(t, indexes, indexConfig.Name)

		// Test document operations
		doc := &driver.Document{
			DocID:    uuid.New().String(), // Generate UUID
			Content:  "This is a test document for RAG integration.",
			Metadata: map[string]interface{}{"type": "test"},
		}

		// Index document
		err = engine.IndexDoc(ctx, indexConfig.Name, doc)
		assert.NoError(t, err)

		// Get document
		retrieved, err := engine.GetDocument(ctx, indexConfig.Name, doc.DocID)
		assert.NoError(t, err)
		assert.Equal(t, doc.Content, retrieved.Content)

		// Search
		results, err := engine.Search(ctx, indexConfig.Name, nil, driver.VectorSearchOptions{
			QueryText: "test document",
			TopK:      5,
		})
		assert.NoError(t, err)
		assert.NotEmpty(t, results)
		assert.Equal(t, doc.DocID, results[0].DocID)

		// Delete document
		err = engine.DeleteDoc(ctx, indexConfig.Name, doc.DocID)
		assert.NoError(t, err)

		// Verify deletion - should return error
		_, err = engine.GetDocument(ctx, indexConfig.Name, doc.DocID)
		assert.Error(t, err)
	})

	// Test batch operations
	t.Run("batch operations", func(t *testing.T) {
		indexName := fmt.Sprintf("%s_batch_%s", baseIndexName, uuid.New().String())
		vectorizeConfig, indexConfig := getTestConfig(t, indexName)

		vectorizer, err := NewVectorizer(DriverOpenAI, vectorizeConfig)
		assert.NoError(t, err)
		defer vectorizer.Close()

		engine, err := NewEngine(DriverQdrant, indexConfig, vectorizer)
		assert.NoError(t, err)
		defer engine.Close()

		err = engine.CreateIndex(ctx, indexConfig)
		assert.NoError(t, err)
		defer engine.DeleteIndex(ctx, indexConfig.Name)

		// Generate UUIDs for batch documents
		id1 := uuid.New().String()
		id2 := uuid.New().String()

		docs := []*driver.Document{
			{
				DocID:    id1,
				Content:  "First batch document",
				Metadata: map[string]interface{}{"index": 1},
			},
			{
				DocID:    id2,
				Content:  "Second batch document",
				Metadata: map[string]interface{}{"index": 2},
			},
		}

		// Batch index
		taskID, err := engine.IndexBatch(ctx, indexConfig.Name, docs)
		assert.NoError(t, err)
		assert.NotEmpty(t, taskID)

		// Check task status
		taskInfo, err := engine.GetTaskInfo(ctx, taskID)
		assert.NoError(t, err)
		assert.NotNil(t, taskInfo)

		// Batch delete
		docIDs := []string{id1, id2}
		deleteTaskID, err := engine.DeleteBatch(ctx, indexConfig.Name, docIDs)
		assert.NoError(t, err)
		assert.NotEmpty(t, deleteTaskID)
	})
}