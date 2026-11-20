package canarynode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

const maxShopifySecretsFileBytes = 1 << 20

// loadShopifySecrets reads a local JSON object keyed by Shopify shop domain.
// The file is optional for local development; when configured, an unknown
// shop is rejected instead of falling back to a global secret.
func loadShopifySecrets(path string) (map[string]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat Shopify secrets file: %w", err)
	}
	if info.IsDir() || info.Size() > maxShopifySecretsFileBytes {
		return nil, errors.New("Shopify secrets file is invalid or too large")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Shopify secrets file must not be readable by group or others")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Shopify secrets file: %w", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode Shopify secrets file: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("Shopify secrets file must contain at least one shop")
	}
	secrets := make(map[string]string, len(raw))
	for shop, secret := range raw {
		shop = normalizeShopDomain(shop)
		secret = strings.TrimSpace(secret)
		if shop == "" || secret == "" || len(secret) > maxSignalStringLength {
			return nil, errors.New("Shopify secrets file contains an invalid shop or secret")
		}
		if _, exists := secrets[shop]; exists {
			return nil, fmt.Errorf("Shopify secrets file contains duplicate shop %q", shop)
		}
		secrets[shop] = secret
	}
	return secrets, nil
}

func normalizeShopDomain(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (s *Server) shopifySecret(shop string) (string, bool) {
	if s.shopifySecrets != nil {
		secret, ok := s.shopifySecrets[normalizeShopDomain(shop)]
		return secret, ok
	}
	if s.secret == "" {
		return "", true
	}
	return s.secret, true
}
