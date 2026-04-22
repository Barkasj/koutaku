package openai

import (
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToOpenAIImageGenerationRequest converts a Koutaku Image Request to OpenAI format
func ToOpenAIImageGenerationRequest(koutakuReq *schemas.KoutakuImageGenerationRequest) *OpenAIImageGenerationRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || koutakuReq.Input.Prompt == "" {
		return nil
	}

	req := &OpenAIImageGenerationRequest{
		Model:  koutakuReq.Model,
		Prompt: koutakuReq.Input.Prompt,
	}

	if koutakuReq.Params != nil {
		req.ImageGenerationParameters = *koutakuReq.Params
	}

	switch koutakuReq.Provider {
	case schemas.XAI:
		filterXAISpecificParameters(req)
	case schemas.OpenAI, schemas.Azure:
		filterOpenAISpecificParameters(req)
	}
	if koutakuReq.Params != nil {
		req.ExtraParams = koutakuReq.Params.ExtraParams
	}
	return req
}

func filterXAISpecificParameters(req *OpenAIImageGenerationRequest) {
	req.ImageGenerationParameters.Quality = nil
	req.ImageGenerationParameters.Style = nil
	req.ImageGenerationParameters.Size = nil
	req.ImageGenerationParameters.OutputCompression = nil
}

func filterOpenAISpecificParameters(req *OpenAIImageGenerationRequest) {
	req.ImageGenerationParameters.Seed = nil
	req.NumInferenceSteps = nil
	req.NegativePrompt = nil
}

// ToKoutakuImageGenerationRequest converts an OpenAI image generation request to Koutaku format
func (request *OpenAIImageGenerationRequest) ToKoutakuImageGenerationRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuImageGenerationRequest {
	if request == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(request.Model, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuImageGenerationRequest{
		Provider: provider,
		Model:    model,
		Input: &schemas.ImageGenerationInput{
			Prompt: request.Prompt,
		},
		Params:    &request.ImageGenerationParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

func (request *OpenAIImageEditRequest) ToKoutakuImageEditRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuImageEditRequest {
	if request == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(request.Model, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuImageEditRequest{
		Provider:  provider,
		Model:     model,
		Input:     request.Input,
		Params:    &request.ImageEditParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

func (request *OpenAIImageVariationRequest) ToKoutakuImageVariationRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuImageVariationRequest {
	if request == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(request.Model, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuImageVariationRequest{
		Provider:  provider,
		Model:     model,
		Input:     request.Input,
		Params:    &request.ImageVariationParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

func ToOpenAIImageEditRequest(koutakuReq *schemas.KoutakuImageEditRequest) *OpenAIImageEditRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || koutakuReq.Input.Images == nil || koutakuReq.Input.Prompt == "" {
		return nil
	}

	req := &OpenAIImageEditRequest{
		Model: koutakuReq.Model,
		Input: koutakuReq.Input,
	}

	if koutakuReq.Params != nil {
		req.ImageEditParameters = *koutakuReq.Params
	}

	if koutakuReq.Params != nil {
		req.ExtraParams = koutakuReq.Params.ExtraParams
	}

	return req
}

func parseImageEditFormDataBodyFromRequest(writer *multipart.Writer, openaiReq *OpenAIImageEditRequest, providerName schemas.ModelProvider) *schemas.KoutakuError {
	// Add model field (required)
	if err := writer.WriteField("model", openaiReq.Model); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to write model field", err)
	}

	// Add prompt field (required)
	if err := writer.WriteField("prompt", openaiReq.Input.Prompt); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to write prompt field", err)
	}

	// Add stream field when requesting streaming
	if openaiReq.Stream != nil && *openaiReq.Stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write stream field", err)
		}
	}

	// Add optional parameters before file parts so routing metadata arrives first upstream.
	if openaiReq.N != nil {
		if err := writer.WriteField("n", strconv.Itoa(*openaiReq.N)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write n field", err)
		}
	}

	if openaiReq.Size != nil {
		if err := writer.WriteField("size", *openaiReq.Size); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write size field", err)
		}
	}

	if openaiReq.ResponseFormat != nil {
		if err := writer.WriteField("response_format", *openaiReq.ResponseFormat); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write response_format field", err)
		}
	}

	if openaiReq.Quality != nil {
		if err := writer.WriteField("quality", *openaiReq.Quality); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write quality field", err)
		}
	}

	if openaiReq.Background != nil {
		if err := writer.WriteField("background", *openaiReq.Background); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write background field", err)
		}
	}

	if openaiReq.InputFidelity != nil {
		if err := writer.WriteField("input_fidelity", *openaiReq.InputFidelity); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write input_fidelity field", err)
		}
	}

	if openaiReq.PartialImages != nil {
		if err := writer.WriteField("partial_images", strconv.Itoa(*openaiReq.PartialImages)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write partial_images field", err)
		}
	}

	if openaiReq.OutputFormat != nil {
		if err := writer.WriteField("output_format", *openaiReq.OutputFormat); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write output_format field", err)
		}
	}

	if openaiReq.OutputCompression != nil {
		if err := writer.WriteField("output_compression", strconv.Itoa(*openaiReq.OutputCompression)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write output_compression field", err)
		}
	}

	if openaiReq.User != nil {
		if err := writer.WriteField("user", *openaiReq.User); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write user field", err)
		}
	}

	// Add image[] fields (one for each image)
	for i, imageInput := range openaiReq.Input.Images {
		fieldName := "image[]"

		// Detect and validate MIME type
		mimeType := http.DetectContentType(imageInput.Image)
		// Fallback to PNG if content type is undetectable or generic
		if mimeType == "" || mimeType == "application/octet-stream" {
			mimeType = "image/png"
		}

		// Determine filename based on MIME type
		var filename string
		switch mimeType {
		case "image/jpeg":
			filename = fmt.Sprintf("image%d.jpg", i)
		case "image/webp":
			filename = fmt.Sprintf("image%d.webp", i)
		default:
			filename = fmt.Sprintf("image%d.png", i)
		}

		// Create form part with proper Content-Type header (not CreateFormFile which defaults to application/octet-stream)
		part, err := writer.CreatePart(map[string][]string{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, filename)},
			"Content-Type":        {mimeType},
		})
		if err != nil {
			return providerUtils.NewKoutakuOperationError(fmt.Sprintf("failed to create form part for image %d", i), err)
		}
		if _, err := part.Write(imageInput.Image); err != nil {
			return providerUtils.NewKoutakuOperationError(fmt.Sprintf("failed to write image %d data", i), err)
		}
	}

	// Add mask if present
	if len(openaiReq.Mask) > 0 {
		// Detect MIME type for mask
		maskMimeType := http.DetectContentType(openaiReq.Mask)
		if maskMimeType != "image/png" && maskMimeType != "image/jpeg" && maskMimeType != "image/webp" {
			maskMimeType = "image/png"
		}

		var maskFilename string
		switch maskMimeType {
		case "image/jpeg":
			maskFilename = "mask.jpg"
		case "image/webp":
			maskFilename = "mask.webp"
		default:
			maskFilename = "mask.png"
		}

		// Create form part with proper Content-Type header
		maskPart, err := writer.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="mask"; filename="` + maskFilename + `"`},
			"Content-Type":        {maskMimeType},
		})
		if err != nil {
			return providerUtils.NewKoutakuOperationError("failed to create mask form part", err)
		}
		if _, err := maskPart.Write(openaiReq.Mask); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write mask data", err)
		}
	}

	// Close the multipart writer
	if err := writer.Close(); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to close multipart writer", err)
	}

	return nil
}

func ToOpenAIImageVariationRequest(koutakuReq *schemas.KoutakuImageVariationRequest) *OpenAIImageVariationRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || koutakuReq.Input.Image.Image == nil || len(koutakuReq.Input.Image.Image) == 0 {
		return nil
	}

	req := &OpenAIImageVariationRequest{
		Model: koutakuReq.Model,
		Input: koutakuReq.Input,
	}

	if koutakuReq.Params != nil {
		req.ImageVariationParameters = *koutakuReq.Params
	}

	if koutakuReq.Params != nil {
		req.ExtraParams = koutakuReq.Params.ExtraParams
	}

	return req
}

func parseImageVariationFormDataBodyFromRequest(writer *multipart.Writer, openaiReq *OpenAIImageVariationRequest, providerName schemas.ModelProvider) *schemas.KoutakuError {
	// Add model field (required)
	if err := writer.WriteField("model", openaiReq.Model); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to write model field", err)
	}

	// Add image file (required)
	if openaiReq.Input == nil || openaiReq.Input.Image.Image == nil || len(openaiReq.Input.Image.Image) == 0 {
		return providerUtils.NewKoutakuOperationError("image is required", nil)
	}

	// Add optional parameters before the image part so metadata arrives first upstream.
	if openaiReq.N != nil {
		if err := writer.WriteField("n", strconv.Itoa(*openaiReq.N)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write n field", err)
		}
	}

	if openaiReq.ResponseFormat != nil {
		if err := writer.WriteField("response_format", *openaiReq.ResponseFormat); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write response_format field", err)
		}
	}

	if openaiReq.Size != nil {
		if err := writer.WriteField("size", *openaiReq.Size); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write size field", err)
		}
	}

	if openaiReq.User != nil {
		if err := writer.WriteField("user", *openaiReq.User); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write user field", err)
		}
	}

	// Detect MIME type
	mimeType := http.DetectContentType(openaiReq.Input.Image.Image)
	// If still not detected, default to PNG
	if mimeType == "application/octet-stream" || mimeType == "" {
		mimeType = "image/png"
	}

	filename := "image"
	part, err := writer.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="image"; filename="%s"`, filename)},
		"Content-Type":        {mimeType},
	})
	if err != nil {
		return providerUtils.NewKoutakuOperationError("failed to create image part", err)
	}

	if _, err := part.Write(openaiReq.Input.Image.Image); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to write image data", err)
	}

	// Close the multipart writer
	if err := writer.Close(); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to close multipart writer", err)
	}

	return nil
}
