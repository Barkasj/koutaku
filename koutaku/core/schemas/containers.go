// Package schemas defines the core schemas and types used by the Koutaku system.
package schemas

// ContainerStatus represents the status of a container.
type ContainerStatus string

const (
	ContainerStatusRunning ContainerStatus = "running"
)

// ContainerExpiresAfter represents the expiration configuration for a container.
type ContainerExpiresAfter struct {
	Anchor  string `json:"anchor"`  // The anchor point for expiration (e.g., "last_active_at")
	Minutes int    `json:"minutes"` // Number of minutes after anchor point
}

// ContainerObject represents a container object returned by the API.
type ContainerObject struct {
	ID           string                 `json:"id"`
	Object       string                 `json:"object,omitempty"` // "container"
	Name         string                 `json:"name"`
	CreatedAt    int64                  `json:"created_at"`
	Status       ContainerStatus        `json:"status,omitempty"`
	ExpiresAfter *ContainerExpiresAfter `json:"expires_after,omitempty"`
	LastActiveAt *int64                 `json:"last_active_at,omitempty"`
	MemoryLimit  string                 `json:"memory_limit,omitempty"` // e.g., "1g", "4g"
	Metadata     map[string]string      `json:"metadata,omitempty"`
}

// KoutakuContainerCreateRequest represents a request to create a container.
type KoutakuContainerCreateRequest struct {
	Provider ModelProvider `json:"provider"`

	// Required fields
	Name string `json:"name"` // Name of the container

	// Optional fields
	ExpiresAfter *ContainerExpiresAfter `json:"expires_after,omitempty"` // Expiration configuration
	FileIDs      []string               `json:"file_ids,omitempty"`      // IDs of existing files to copy into this container
	MemoryLimit  string                 `json:"memory_limit,omitempty"`  // Memory limit (e.g., "1g", "4g")
	Metadata     map[string]string      `json:"metadata,omitempty"`      // User-provided metadata

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerCreateResponse represents the response from creating a container.
type KoutakuContainerCreateResponse struct {
	ID           string                 `json:"id"`
	Object       string                 `json:"object,omitempty"` // "container"
	Name         string                 `json:"name"`
	CreatedAt    int64                  `json:"created_at"`
	Status       ContainerStatus        `json:"status,omitempty"`
	ExpiresAfter *ContainerExpiresAfter `json:"expires_after,omitempty"`
	LastActiveAt *int64                 `json:"last_active_at,omitempty"`
	MemoryLimit  string                 `json:"memory_limit,omitempty"`
	Metadata     map[string]string      `json:"metadata,omitempty"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerListRequest represents a request to list containers.
type KoutakuContainerListRequest struct {
	Provider ModelProvider `json:"provider"`

	// Pagination
	Limit int     `json:"limit,omitempty"` // Max results to return (1-100, default 20)
	After *string `json:"after,omitempty"` // Cursor for pagination
	Order *string `json:"order,omitempty"` // Sort order (asc/desc), default desc

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerListResponse represents the response from listing containers.
type KoutakuContainerListResponse struct {
	Object  string            `json:"object,omitempty"` // "list"
	Data    []ContainerObject `json:"data"`
	FirstID *string           `json:"first_id,omitempty"`
	LastID  *string           `json:"last_id,omitempty"`
	HasMore bool              `json:"has_more,omitempty"`
	After   *string           `json:"after,omitempty"` // Encoded cursor for next page (includes key index for multi-key pagination)

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerRetrieveRequest represents a request to retrieve a container.
type KoutakuContainerRetrieveRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container to retrieve

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerRetrieveResponse represents the response from retrieving a container.
type KoutakuContainerRetrieveResponse struct {
	ID           string                 `json:"id"`
	Object       string                 `json:"object,omitempty"` // "container"
	Name         string                 `json:"name"`
	CreatedAt    int64                  `json:"created_at"`
	Status       ContainerStatus        `json:"status,omitempty"`
	ExpiresAfter *ContainerExpiresAfter `json:"expires_after,omitempty"`
	LastActiveAt *int64                 `json:"last_active_at,omitempty"`
	MemoryLimit  string                 `json:"memory_limit,omitempty"`
	Metadata     map[string]string      `json:"metadata,omitempty"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerDeleteRequest represents a request to delete a container.
type KoutakuContainerDeleteRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container to delete

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerDeleteResponse represents the response from deleting a container.
type KoutakuContainerDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"` // "container.deleted"
	Deleted bool   `json:"deleted"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// =============================================================================
// CONTAINER FILES API
// =============================================================================

// ContainerFileObject represents a file within a container.
type ContainerFileObject struct {
	ID          string `json:"id"`
	Object      string `json:"object,omitempty"` // "container.file"
	Bytes       int64  `json:"bytes"`
	CreatedAt   int64  `json:"created_at"`
	ContainerID string `json:"container_id"`
	Path        string `json:"path"`
	Source      string `json:"source"` // "user" typically
}

// KoutakuContainerFileCreateRequest represents a request to create a file in a container.
type KoutakuContainerFileCreateRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container

	// One of these must be provided
	File   []byte  `json:"-"`                    // File content (for multipart upload)
	FileID *string `json:"file_id,omitempty"`    // Reference to existing file
	Path   *string `json:"file_path,omitempty"`  // Path for the file in the container

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerFileCreateResponse represents the response from creating a container file.
type KoutakuContainerFileCreateResponse struct {
	ID          string `json:"id"`
	Object      string `json:"object,omitempty"` // "container.file"
	Bytes       int64  `json:"bytes"`
	CreatedAt   int64  `json:"created_at"`
	ContainerID string `json:"container_id"`
	Path        string `json:"path"`
	Source      string `json:"source"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerFileListRequest represents a request to list files in a container.
type KoutakuContainerFileListRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container

	// Pagination
	Limit int     `json:"limit,omitempty"` // Max results to return (1-100, default 20)
	After *string `json:"after,omitempty"` // Cursor for pagination
	Order *string `json:"order,omitempty"` // Sort order (asc/desc), default desc

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerFileListResponse represents the response from listing container files.
type KoutakuContainerFileListResponse struct {
	Object  string                `json:"object,omitempty"` // "list"
	Data    []ContainerFileObject `json:"data"`
	FirstID *string               `json:"first_id,omitempty"`
	LastID  *string               `json:"last_id,omitempty"`
	HasMore bool                  `json:"has_more,omitempty"`
	After   *string               `json:"after,omitempty"` // Encoded cursor for next page (includes key index for multi-key pagination)

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerFileRetrieveRequest represents a request to retrieve a container file.
type KoutakuContainerFileRetrieveRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container
	FileID      string        `json:"file_id"`      // ID of the file to retrieve

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerFileRetrieveResponse represents the response from retrieving a container file.
type KoutakuContainerFileRetrieveResponse struct {
	ID          string `json:"id"`
	Object      string `json:"object,omitempty"` // "container.file"
	Bytes       int64  `json:"bytes"`
	CreatedAt   int64  `json:"created_at"`
	ContainerID string `json:"container_id"`
	Path        string `json:"path"`
	Source      string `json:"source"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerFileContentRequest represents a request to retrieve the content of a container file.
type KoutakuContainerFileContentRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container
	FileID      string        `json:"file_id"`      // ID of the file

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerFileContentResponse represents the response from retrieving container file content.
type KoutakuContainerFileContentResponse struct {
	Content     []byte `json:"content"`      // Raw file content
	ContentType string `json:"content_type"` // MIME type of the content

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}

// KoutakuContainerFileDeleteRequest represents a request to delete a container file.
type KoutakuContainerFileDeleteRequest struct {
	Provider    ModelProvider `json:"provider"`
	ContainerID string        `json:"container_id"` // ID of the container
	FileID      string        `json:"file_id"`      // ID of the file to delete

	// Extra parameters for provider-specific features
	ExtraParams map[string]interface{} `json:"-"`
}

// KoutakuContainerFileDeleteResponse represents the response from deleting a container file.
type KoutakuContainerFileDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"` // "container.file.deleted"
	Deleted bool   `json:"deleted"`

	ExtraFields KoutakuResponseExtraFields `json:"extra_fields"`
}
