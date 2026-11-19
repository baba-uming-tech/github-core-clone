package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	requestTimeoutMS = 30 * time.Second
	retryInitial     = 250 * time.Millisecond
	retryMax         = 2 * time.Second
)

func sleep(ms time.Duration) {
	time.Sleep(ms)
}

func retryDelay(attempt int) time.Duration {
	exp := attempt
	if exp > 8 {
		exp = 8
	}
	delay := retryInitial * time.Duration(1<<uint(exp))
	if delay > retryMax {
		return retryMax
	}
	return delay
}

func register(envURL string, port int) error {
	body, err := json.Marshal(struct {
		Port int `json:"port"`
	}{Port: port})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: requestTimeoutMS}
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeoutMS)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if reqErr == nil {
			req.Header.Set("Content-Type", "application/json")
			res, resErr := client.Do(req)
			if resErr == nil {
				resBody, _ := io.ReadAll(res.Body)
				res.Body.Close()
				status := res.StatusCode
				cancel()
				if status == http.StatusOK || status == http.StatusConflict {
					return nil
				}
				if status == http.StatusBadRequest || status == http.StatusMethodNotAllowed {
					return fmt.Errorf("register answered HTTP %d: %s", status, string(resBody))
				}
			} else {
				cancel()
			}
		} else {
			cancel()
		}
		sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) error {
	client := &http.Client{Timeout: requestTimeoutMS}
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeoutMS)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, envURL+"/done", nil)
		if err == nil {
			res, resErr := client.Do(req)
			if resErr == nil {
				var body struct {
					Done bool `json:"done"`
				}
				decodeErr := json.NewDecoder(res.Body).Decode(&body)
				res.Body.Close()
				cancel()
				if res.StatusCode >= 200 && res.StatusCode < 300 && decodeErr == nil && body.Done {
					return nil
				}
			} else {
				cancel()
			}
		} else {
			cancel()
		}
		sleep(retryDelay(attempt))
	}
}

func run() error {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set: there is no env to register with")
	}

	store := NewStore()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return fmt.Errorf("the server did not bind an ephemeral TCP port")
	}

	server := &http.Server{Handler: NewServer(store)}
	server.SetKeepAlivesEnabled(true)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	port := addr.Port
	os.Stdout.WriteString(fmt.Sprintf("[entry] listening on port %d\n", port))

	if err := register(envURL, port); err != nil {
		server.Close()
		return err
	}
	os.Stdout.WriteString("[entry] registered; waiting for the env to say done\n")

	if err := waitForDone(envURL); err != nil {
		server.Close()
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		server.Close()
	}
	os.Stdout.WriteString("[entry] done\n")
	return nil
}

func main() {
	if err := run(); err != nil {
		os.Stderr.WriteString("[entry] " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}
