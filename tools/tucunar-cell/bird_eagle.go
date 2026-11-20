package canarynode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxEagleBytes = 1 << 20

var eagleSHA256 = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func init() {
	registerCapabilityExecutor(capabilityEagle, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runEagle) })
}

func runEagle(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[EagleExtractRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.ArtifactURI) == "" || !eagleSHA256.MatchString(strings.TrimSpace(p.SHA256)) {
		return nil, fmt.Errorf("artifact_uri and a 64-character sha256 are required")
	}
	result := EagleExtractResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityEagle), Operation: "extract", Status: "completed"}}
	path := strings.TrimPrefix(p.ArtifactURI, "file://")
	if strings.Contains(path, "\x00") {
		return nil, fmt.Errorf("invalid artifact URI")
	}
	if path == p.ArtifactURI && !filepath.IsAbs(path) {
		return nil, fmt.Errorf("only absolute local artifact paths are supported")
	}
	ctx, span := tracer.Start(ctx, "eagle.file_extract", withAttributes(spanString("tucunar.artifact_uri", p.ArtifactURI)))
	defer span.End()
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxEagleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if len(data) > maxEagleBytes {
		return nil, fmt.Errorf("artifact exceeds bounded extraction size")
	}
	h := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(h[:]), p.SHA256) {
		return nil, fmt.Errorf("artifact sha256 mismatch")
	}
	prov := []string{p.ArtifactURI, "sha256:" + strings.ToLower(p.SHA256)}
	if strings.Contains(strings.ToLower(p.MediaType), "json") || json.Valid(data) {
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode JSON artifact: %w", err)
		}
		result.Facts = append(result.Facts, Fact{Key: "artifact", Value: value, Confidence: 1, Provenance: prov})
	} else {
		text := strings.TrimSpace(string(data))
		if text != "" {
			result.Facts = append(result.Facts, Fact{Key: "text", Value: text, Confidence: 1, Provenance: prov})
		}
	}
	return result, nil
}
