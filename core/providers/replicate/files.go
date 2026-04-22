package replicate

import (
	"time"

	"github.com/koutaku/koutaku/core/schemas"
)

// Replicate File API Converters

// ToKoutakuFileStatus converts Replicate file status to Koutaku file status.
// Replicate doesn't explicitly provide status, so we infer from the response.
func ToKoutakuFileStatus(fileResp *ReplicateFileResponse) schemas.FileStatus {
	// If file has all required fields and is accessible, it's processed
	if fileResp.ID != "" && fileResp.Size > 0 {
		return schemas.FileStatusProcessed
	}
	return schemas.FileStatusUploaded
}

// ToKoutakuFileUploadResponse converts Replicate file response to Koutaku file upload response.
func (r *ReplicateFileResponse) ToKoutakuFileUploadResponse(providerName schemas.ModelProvider, latency time.Duration, sendBackRawRequest bool, sendBackRawResponse bool, rawRequest interface{}, rawResponse interface{}) *schemas.KoutakuFileUploadResponse {
	resp := &schemas.KoutakuFileUploadResponse{
		ID:             r.ID,
		Object:         "file",
		Bytes:          r.Size,
		CreatedAt:      ParseReplicateTimestamp(r.CreatedAt),
		Filename:       r.Name,
		Purpose:        schemas.FilePurposeBatch, // Replicate uses files primarily for batch/general purposes
		Status:         ToKoutakuFileStatus(r),
		StorageBackend: schemas.FileStorageAPI,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency:     latency.Milliseconds(),
		},
	}

	// Add ExpiresAt if present
	if r.ExpiresAt != "" {
		expiresAt := ParseReplicateTimestamp(r.ExpiresAt)
		if expiresAt > 0 {
			resp.ExpiresAt = &expiresAt
		}
	}

	if sendBackRawRequest {
		resp.ExtraFields.RawRequest = rawRequest
	}

	if sendBackRawResponse {
		resp.ExtraFields.RawResponse = rawResponse
	}

	return resp
}

// ToKoutakuFileRetrieveResponse converts Replicate file response to Koutaku file retrieve response.
func (r *ReplicateFileResponse) ToKoutakuFileRetrieveResponse(providerName schemas.ModelProvider, latency time.Duration, sendBackRawRequest bool, sendBackRawResponse bool, rawRequest interface{}, rawResponse interface{}) *schemas.KoutakuFileRetrieveResponse {
	resp := &schemas.KoutakuFileRetrieveResponse{
		ID:             r.ID,
		Object:         "file",
		Bytes:          r.Size,
		CreatedAt:      ParseReplicateTimestamp(r.CreatedAt),
		Filename:       r.Name,
		Purpose:        schemas.FilePurposeBatch,
		Status:         ToKoutakuFileStatus(r),
		StorageBackend: schemas.FileStorageAPI,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency:     latency.Milliseconds(),
		},
	}

	// Add ExpiresAt if present
	if r.ExpiresAt != "" {
		expiresAt := ParseReplicateTimestamp(r.ExpiresAt)
		if expiresAt > 0 {
			resp.ExpiresAt = &expiresAt
		}
	}

	if sendBackRawRequest {
		resp.ExtraFields.RawRequest = rawRequest
	}

	if sendBackRawResponse {
		resp.ExtraFields.RawResponse = rawResponse
	}

	return resp
}
