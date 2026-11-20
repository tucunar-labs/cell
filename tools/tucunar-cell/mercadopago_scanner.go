package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMercadoPagoAPIBaseURL = "https://api.mercadopago.com"
	defaultMercadoPagoTenantID   = "mercadolibre-test"
	defaultMercadoPagoPageSize   = 50
	maxMercadoPagoPageSize       = 50
	maxMercadoPagoPages          = 20
	defaultMercadoPagoLookback   = 24 * time.Hour
	defaultMercadoPagoInterval   = time.Minute
)

type signalWriter interface {
	InsertSignals(context.Context, []Signal) error
}

type mercadoPagoScannerConfig struct {
	AccessToken  string
	APIBaseURL   string
	TenantID     string
	PollInterval time.Duration
	Lookback     time.Duration
	PageSize     int
	HTTPTimeout  time.Duration
}

type mercadoPagoScanner struct {
	writer signalWriter
	config mercadoPagoScannerConfig
	client *http.Client
	now    func() time.Time
}

type mercadoPagoPaymentsResponse struct {
	Results []mercadoPagoPayment `json:"results"`
	Paging  struct {
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"paging"`
}

type mercadoPagoPayment struct {
	ID                json.RawMessage `json:"id"`
	Status            string          `json:"status"`
	DateCreated       string          `json:"date_created"`
	DateApproved      string          `json:"date_approved"`
	TransactionAmount float64         `json:"transaction_amount"`
	CurrencyID        string          `json:"currency_id"`
}

type mercadoPagoSignalPayload struct {
	Status            string  `json:"status"`
	TransactionAmount float64 `json:"transaction_amount"`
	CurrencyID        string  `json:"currency_id"`
	DateCreated       string  `json:"date_created"`
	DateApproved      string  `json:"date_approved,omitempty"`
}

func loadMercadoPagoScannerConfig() (*mercadoPagoScannerConfig, error) {
	token := strings.TrimSpace(os.Getenv("MERCADOPAGO_ACCESS_TOKEN"))
	if token == "" {
		return nil, nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(env("MERCADOPAGO_API_BASE_URL", defaultMercadoPagoAPIBaseURL)), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("MERCADOPAGO_API_BASE_URL must be an absolute URL")
	}
	if parsed.Scheme != "https" && env("MERCADOPAGO_ALLOW_INSECURE_HTTP", "") != "1" {
		return nil, errors.New("MERCADOPAGO_API_BASE_URL must use HTTPS")
	}
	pageSize := intFromEnv("MERCADOPAGO_PAGE_SIZE", defaultMercadoPagoPageSize)
	if pageSize > maxMercadoPagoPageSize {
		pageSize = maxMercadoPagoPageSize
	}
	lookbackMinutes := intFromEnv("MERCADOPAGO_LOOKBACK_MINUTES", int(defaultMercadoPagoLookback/time.Minute))
	if lookbackMinutes < 1 {
		lookbackMinutes = int(defaultMercadoPagoLookback / time.Minute)
	}
	tenantID := strings.TrimSpace(env("MERCADOPAGO_TENANT_ID", defaultMercadoPagoTenantID))
	if tenantID == "" {
		tenantID = defaultMercadoPagoTenantID
	}
	return &mercadoPagoScannerConfig{
		AccessToken:  token,
		APIBaseURL:   baseURL,
		TenantID:     tenantID,
		PollInterval: durationFromEnv("MERCADOPAGO_POLL_INTERVAL_SECONDS", defaultMercadoPagoInterval),
		Lookback:     time.Duration(lookbackMinutes) * time.Minute,
		PageSize:     pageSize,
		HTTPTimeout:  15 * time.Second,
	}, nil
}

func newMercadoPagoScanner(writer signalWriter, config *mercadoPagoScannerConfig) *mercadoPagoScanner {
	if config == nil {
		return nil
	}
	if config.PageSize < 1 || config.PageSize > maxMercadoPagoPageSize {
		config.PageSize = defaultMercadoPagoPageSize
	}
	if config.HTTPTimeout <= 0 {
		config.HTTPTimeout = 15 * time.Second
	}
	return &mercadoPagoScanner{
		writer: writer,
		config: *config,
		client: &http.Client{Timeout: config.HTTPTimeout},
		now:    time.Now,
	}
}

func (s *mercadoPagoScanner) Run(ctx context.Context) {
	if s == nil {
		return
	}
	if err := s.syncOnce(ctx); err != nil {
		logger.Warn("Mercado Pago scan failed", "error", err, "tenant_id", s.config.TenantID)
	}
	interval := s.config.PollInterval
	if interval < time.Second {
		interval = defaultMercadoPagoInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.syncOnce(ctx); err != nil {
				logger.Warn("Mercado Pago scan failed", "error", err, "tenant_id", s.config.TenantID)
			}
		}
	}
}

func (s *mercadoPagoScanner) syncOnce(ctx context.Context) error {
	if s.writer == nil {
		return errors.New("Mercado Pago scanner has no signal writer")
	}
	now := s.now()
	if now.IsZero() {
		now = time.Now()
	}
	begin := now.UTC().Add(-s.config.Lookback)
	end := now.UTC()
	offset := 0
	for page := 0; page < maxMercadoPagoPages; page++ {
		response, err := s.fetchPage(ctx, begin, end, offset)
		if err != nil {
			return err
		}
		signals, err := mercadoPagoPaymentsToSignals(response.Results, s.config.TenantID, now.UTC())
		if err != nil {
			return err
		}
		if len(signals) > 0 {
			if err := s.writer.InsertSignals(ctx, signals); err != nil {
				return fmt.Errorf("insert Mercado Pago signals: %w", err)
			}
		}
		if len(response.Results) == 0 || offset+len(response.Results) >= response.Paging.Total {
			return nil
		}
		offset += len(response.Results)
	}
	return fmt.Errorf("Mercado Pago search exceeded %d pages", maxMercadoPagoPages)
}

func (s *mercadoPagoScanner) fetchPage(ctx context.Context, begin, end time.Time, offset int) (mercadoPagoPaymentsResponse, error) {
	parsed, err := url.Parse(s.config.APIBaseURL + "/v1/payments/search")
	if err != nil {
		return mercadoPagoPaymentsResponse{}, err
	}
	query := parsed.Query()
	query.Set("sort", "date_created")
	query.Set("criteria", "desc")
	query.Set("range", "date_created")
	query.Set("begin_date", begin.UTC().Format(time.RFC3339))
	query.Set("end_date", end.UTC().Format(time.RFC3339))
	query.Set("limit", strconv.Itoa(s.config.PageSize))
	query.Set("offset", strconv.Itoa(offset))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return mercadoPagoPaymentsResponse{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.config.AccessToken)
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return mercadoPagoPaymentsResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return mercadoPagoPaymentsResponse{}, fmt.Errorf("Mercado Pago API returned HTTP %d", response.StatusCode)
	}
	var payload mercadoPagoPaymentsResponse
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return mercadoPagoPaymentsResponse{}, fmt.Errorf("decode Mercado Pago response: %w", err)
	}
	return payload, nil
}

func mercadoPagoPaymentsToSignals(payments []mercadoPagoPayment, tenantID string, fallback time.Time) ([]Signal, error) {
	result := make([]Signal, 0, len(payments))
	for _, payment := range payments {
		id := providerID(payment.ID)
		if id == "" {
			return nil, errors.New("Mercado Pago payment is missing id")
		}
		status := strings.ToLower(strings.TrimSpace(payment.Status))
		if status == "" {
			status = "unknown"
		}
		occurredAt, err := parseMercadoPagoTime(payment.DateApproved)
		if err != nil {
			occurredAt, err = parseMercadoPagoTime(payment.DateCreated)
		}
		if err != nil {
			if fallback.IsZero() {
				return nil, fmt.Errorf("payment %s has no valid timestamp", id)
			}
			occurredAt = fallback
		}
		payload, err := json.Marshal(mercadoPagoSignalPayload{
			Status:            status,
			TransactionAmount: payment.TransactionAmount,
			CurrencyID:        strings.ToUpper(strings.TrimSpace(payment.CurrencyID)),
			DateCreated:       payment.DateCreated,
			DateApproved:      payment.DateApproved,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, Signal{
			ExternalID: id,
			Source:     "mercadopago",
			TenantID:   tenantID,
			EntityID:   id,
			Kind:       "payment",
			Name:       mercadoPagoSignalName(status),
			Value:      payment.TransactionAmount,
			Unit:       strings.ToUpper(strings.TrimSpace(payment.CurrencyID)),
			Payload:    payload,
			OccurredAt: occurredAt,
		})
	}
	return result, nil
}

func mercadoPagoSignalName(status string) string {
	if status == "approved" {
		return "payment_success"
	}
	return "payment_" + status
}

func providerID(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var decoded string
		if json.Unmarshal(raw, &decoded) == nil {
			value = strings.TrimSpace(decoded)
		}
	}
	return value
}

func parseMercadoPagoTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
