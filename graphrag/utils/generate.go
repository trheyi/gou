package utils

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yaoapp/gou/graphrag/types"
)

// GenDocID generates a UUID for document identification (without dashes)
func GenDocID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// GenDocIDWithCollectionID generates a UUID for document identification with GraphName prefix
// Format: graphname__uuid (without dashes)
func GenDocIDWithCollectionID(graphName string) string {
	if graphName == "" {
		graphName = "default"
	}
	// Clean the graph name to ensure it's valid
	cleanGraphName := strings.ToLower(strings.TrimSpace(graphName))
	uuid := strings.ReplaceAll(uuid.New().String(), "-", "")
	return fmt.Sprintf("%s__%s", cleanGraphName, uuid)
}

// ExtractCollectionIDFromDocID extracts the GraphName from a prefixed docID
// Returns the GraphName and the plain UUID part
func ExtractCollectionIDFromDocID(docID string) (string, string) {
	if docID == "" {
		return "", ""
	}

	// Check if docID contains the separator "__"
	parts := strings.SplitN(docID, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1] // graphName, uuid
	}

	// If no separator found, assume it's a plain UUID without prefix
	return "", docID
}

// IsDocIDWithCollectionID checks if a docID contains a GraphName prefix
func IsDocIDWithCollectionID(docID string) bool {
	return strings.Contains(docID, "__")
}

// GenChunkID generates a UUID for chunk identification
func GenChunkID() string {
	return uuid.New().String()
}

// GenShortID generates a shorter ID using timestamp and random bytes
// Useful for cases where shorter IDs are preferred
func GenShortID() string {
	timestamp := time.Now().Unix()

	// Generate 4 random bytes
	randomBytes := make([]byte, 4)
	rand.Read(randomBytes)

	return fmt.Sprintf("%x_%x", timestamp, randomBytes)
}

// BatchGenDocIDs generates multiple document IDs at once
func BatchGenDocIDs(count int) []string {
	if count <= 0 {
		return []string{}
	}

	docIDs := make([]string, count)
	for i := 0; i < count; i++ {
		docIDs[i] = GenDocID()
	}
	return docIDs
}

// BatchGenChunkIDs generates multiple chunk IDs at once
func BatchGenChunkIDs(count int) []string {
	if count <= 0 {
		return []string{}
	}

	chunkIDs := make([]string, count)
	for i := 0; i < count; i++ {
		chunkIDs[i] = GenChunkID()
	}
	return chunkIDs
}

// IsValidUUID checks if a string is a valid UUID format
func IsValidUUID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// ValidateName validates collection name format
// Only allows a-z, A-Z, 0-9, and underscore characters
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("collection name cannot be empty")
	}

	validNamePattern := regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
	if !validNamePattern.MatchString(name) {
		return fmt.Errorf("invalid collection name format: '%s', only letters, numbers, and underscores are allowed", name)
	}

	return nil
}

// GenCollectionID generates a unique collection ID with prefix + timestamp
func GenCollectionID(prefix string) string {
	timestamp := time.Now().UnixNano()
	return fmt.Sprintf("%s%d", prefix, timestamp)
}

// GetCollectionIDs generates simple collection IDs: name_vector, name_graph, name_store
func GetCollectionIDs(name string) (types.CollectionIDs, error) {
	if err := ValidateName(name); err != nil {
		return types.CollectionIDs{}, err
	}

	cleanName := strings.ToLower(name)

	return types.CollectionIDs{
		Vector: fmt.Sprintf("%s_vector", cleanName),
		Graph:  fmt.Sprintf("%s_graph", cleanName),
		Store:  fmt.Sprintf("%s_store", cleanName),
	}, nil
}

// ExtractCollectionIDFromVectorName extracts the original collection ID from a vector collection name
// This is the reverse operation of GetCollectionIDs
func ExtractCollectionIDFromVectorName(vectorName string) string {
	vectorName = strings.TrimSpace(vectorName)
	if vectorName == "" {
		return ""
	}

	// Check if the vector name ends with "_vector"
	vectorSuffix := "_vector"
	if strings.HasSuffix(vectorName, vectorSuffix) {
		// Remove the "_vector" suffix to get the original collection ID
		return strings.TrimSuffix(vectorName, vectorSuffix)
	}

	// If it doesn't follow the expected pattern, return empty string (strict mode)
	return ""
}

// ExtractCollectionIDFromGraphName extracts the original collection ID from a graph collection name
func ExtractCollectionIDFromGraphName(graphName string) string {
	graphName = strings.TrimSpace(graphName)
	if graphName == "" {
		return ""
	}

	// Check if the graph name ends with "_graph"
	graphSuffix := "_graph"
	if strings.HasSuffix(graphName, graphSuffix) {
		// Remove the "_graph" suffix to get the original collection ID
		return strings.TrimSuffix(graphName, graphSuffix)
	}

	// If it doesn't follow the expected pattern, return empty string (strict mode)
	return ""
}

// ExtractCollectionIDFromStoreName extracts the original collection ID from a store collection name
func ExtractCollectionIDFromStoreName(storeName string) string {
	storeName = strings.TrimSpace(storeName)
	if storeName == "" {
		return ""
	}

	// Check if the store name ends with "_store"
	storeSuffix := "_store"
	if strings.HasSuffix(storeName, storeSuffix) {
		// Remove the "_store" suffix to get the original collection ID
		return strings.TrimSuffix(storeName, storeSuffix)
	}

	// If it doesn't follow the expected pattern, return empty string (strict mode)
	return ""
}
