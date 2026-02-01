package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	APIKey string
	Symbol string
	NDays  int
	Port   string
}

func loadConfig() (*Config, error) {
	apiKey := os.Getenv("ALPHA_VANTAGE_API_KEY")
	if apiKey == "" {
		return nil, errors.New("environment variable ALPHA_VANTAGE_API_KEY is required")
	}

	symbol := os.Getenv("SYMBOL")
	if symbol == "" {
		return nil, errors.New("environment variable SYMBOL is required")
	}

	nDaysStr := os.Getenv("NDAYS")
	if nDaysStr == "" {
		return nil, errors.New("environment variable NDAYS is required")
	}
	nDays, err := strconv.Atoi(nDaysStr)
	if err != nil || nDays <= 0 {
		return nil, errors.New("NDAYS must be a positive integer")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	return &Config{
		APIKey: apiKey,
		Symbol: symbol,
		NDays:  nDays,
		Port:   port,
	}, nil
}

type StockResponse struct {
	Symbol       string       `json:"symbol"`
	AveragePrice float64      `json:"average_price"`
	Days         int          `json:"days_calculated"`
	History      []DailyPrice `json:"history"`
}

type DailyPrice struct {
	Date  string  `json:"date"`
	Price float64 `json:"price"`
}

// Custom errors for distinguishing retryable vs non-retryable failures
var (
	ErrRateLimitedPerSecond = errors.New("rate limited: exceeds 1 request per second")
	ErrDailyLimitExceeded   = errors.New("daily API limit exceeded (25 requests/day)")
	ErrNoData               = errors.New("no stock data found in response")
)

func containsAny(s string, substrings ...string) bool {
	lower := strings.ToLower(s)
	for _, substr := range substrings {
		if strings.Contains(lower, strings.ToLower(substr)) {
			return true
		}
	}
	return false
}

// AlphaVantage JSON structure
type avResponse struct {
	TimeSeries map[string]struct {
		Close string `json:"4. close"`
	} `json:"Time Series (Daily)"`
	Note         string `json:"Note"`          // Rate limit message
	Information  string `json:"Information"`   // API information/warnings
	ErrorMessage string `json:"Error Message"` // Error messages
}

// getStockData handles the retry loop and data fetching
func getStockData(ctx context.Context, cfg *Config, logger *slog.Logger) (*StockResponse, error) {
	url := fmt.Sprintf("https://www.alphavantage.co/query?function=TIME_SERIES_DAILY&symbol=%s&apikey=%s", cfg.Symbol, cfg.APIKey)

	const maxRetries = 2
	const baseDelay = 500 * time.Millisecond

	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		client := &http.Client{Timeout: 8 * time.Second}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			logger.Warn("HTTP request failed", "attempt", attempt+1, "error", err)
		} else {
			result, parseErr := func() (*StockResponse, error) {
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					return nil, fmt.Errorf("upstream API status: %d", resp.StatusCode)
				}

				return parseStockData(resp.Body, cfg.NDays, logger)
			}()

			if parseErr != nil {
				if errors.Is(parseErr, ErrDailyLimitExceeded) {
					logger.Error("Daily API limit exceeded - cannot retry",
						"attempt", attempt+1,
						"error", parseErr)
					return nil, parseErr // Return immediately - no point retrying
				}

				if errors.Is(parseErr, ErrRateLimitedPerSecond) {
					lastErr = parseErr
					logger.Warn("Per-second rate limit hit - will retry with backoff",
						"attempt", attempt+1,
						"max_retries", maxRetries+1)
				} else {
					logger.Error("Non-retryable API error", "attempt", attempt+1, "error", parseErr)
					return nil, parseErr
				}
			} else {
				logger.Info("Successfully fetched stock data", "attempt", attempt+1)
				return result, nil
			}
		}

		if attempt < maxRetries {
			backoff := time.Duration(1<<uint(attempt)) * baseDelay
			jitter := time.Duration(rand.Int63n(100)) * time.Millisecond
			totalDelay := backoff + jitter

			logger.Info("Retrying after exponential backoff",
				"current_attempt", attempt+1,
				"next_attempt", attempt+2,
				"total_delay_ms", totalDelay.Milliseconds(),
				"backoff_ms", backoff.Milliseconds(),
				"jitter_ms", jitter.Milliseconds())

			select {
			case <-time.After(totalDelay):
			// Continue to next retry
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	// All retries exhausted
	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries+1, lastErr)
}

func parseStockData(r io.Reader, limit int, logger *slog.Logger) (*StockResponse, error) {
	var raw avResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("failed to decode JSON response: %w", err)
	}

	// Check for API error messages and distinguish between retryable and non-retryable errors
	if raw.Note != "" {
		logger.Warn("Alpha Vantage Note received", "message", raw.Note)
		return nil, fmt.Errorf("%w: %s", ErrRateLimitedPerSecond, raw.Note)
	}

	if raw.Information != "" {
		// Check for the retryable per-second limit FIRST
		if containsAny(raw.Information, "1 request per second", "sparingly") {
			return nil, fmt.Errorf("%w: %s", ErrRateLimitedPerSecond, raw.Information)
		}

		// Check for daily limit secondary (or look for specific "exceeded" phrasing if possible)
		if containsAny(raw.Information, "daily rate limit", "25 requests per day") {
			return nil, fmt.Errorf("%w: %s", ErrDailyLimitExceeded, raw.Information)
		}
	}

	if raw.ErrorMessage != "" {
		logger.Error("Alpha Vantage Error received", "message", raw.ErrorMessage)
		return nil, fmt.Errorf("API error: %s", raw.ErrorMessage)
	}

	if len(raw.TimeSeries) == 0 {
		return nil, ErrNoData
	}

	var history []DailyPrice
	for date, data := range raw.TimeSeries {
		price, _ := strconv.ParseFloat(data.Close, 64)
		history = append(history, DailyPrice{Date: date, Price: price})
	}

	sort.Slice(history, func(i, j int) bool {
		return history[i].Date > history[j].Date
	})

	if len(history) > limit {
		history = history[:limit]
	}

	var sum float64
	for _, day := range history {
		sum += day.Price
	}

	avg := 0.0
	if len(history) > 0 {
		avg = sum / float64(len(history))
	}

	return &StockResponse{
		AveragePrice: avg,
		Days:         len(history),
		History:      history,
	}, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("Startup failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	mux.HandleFunc("/stock", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("Handling request", "path", r.URL.Path)

		resp, err := getStockData(r.Context(), cfg, logger)
		if err != nil {
			logger.Error("Failed to get stock data", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		resp.Symbol = cfg.Symbol

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Addr:        ":" + cfg.Port,
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("Server starting", "port", cfg.Port, "symbol", cfg.Symbol)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server failed to start", "error", err)
			os.Exit(1)
		}
	}()

	<-stop
	logger.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
	}

	logger.Info("Server exited gracefully")
}
