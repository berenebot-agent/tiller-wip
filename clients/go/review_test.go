package gateway_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gateway "github.com/tiller-router/tiller-router/clients/go"
)

func TestErrorBodyTimeoutAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{true, false} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		c := client(t, s, func(cfg *gateway.Config) { cfg.Timeout = 50 * time.Millisecond })
		ctx, cancel := context.WithCancel(context.Background())
		if cancelEarly {
			time.AfterFunc(10*time.Millisecond, cancel)
		}
		_, err := c.Chat(ctx, request())
		cancel()
		var e *gateway.Error
		want := "timeout"
		if cancelEarly {
			want = "cancelled"
		}
		if !errors.As(err, &e) || e.Category != want {
			t.Fatalf("error body category = %v, want %s", err, want)
		}
		s.Close()
	}
}

func TestMalformedStreamChoiceCannotComplete(t *testing.T) {
	for _, choice := range []string{"null", "{}", `{"delta":null}`, `{"delta":{},"index":-1}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":["+choice+"]}\n\ndata: [DONE]\n\n")
		}))
		c := client(t, s, nil)
		stream, err := c.Stream(context.Background(), request())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Next(); err == nil {
			t.Fatal("malformed choice accepted", choice)
		}
		stream.Close()
		s.Close()
	}
}
