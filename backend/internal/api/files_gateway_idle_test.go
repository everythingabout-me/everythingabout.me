package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPartReadIdleRealHTTP(t *testing.T) {
	const idle = 200 * time.Millisecond
	for _, stalled := range []bool{true, false} {
		name := "progress resets deadline and keepalive clears it"
		if stalled {
			name = "stalled body times out"
		}
		t.Run(name, func(t *testing.T) {
			readResult := make(chan error, 1)
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clear := partReadIdle(w, r, idle)
				defer clear()
				if r.Method == http.MethodPut {
					_, err := io.Copy(io.Discard, r.Body)
					readResult <- err
					if err != nil {
						w.Header().Set("Connection", "close")
						w.WriteHeader(408)
						return
					}
				}
				w.WriteHeader(204)
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.Start()
			defer server.Close()
			transport := &http.Transport{MaxConnsPerHost: 1}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			request, err := http.NewRequest("PUT", server.URL+"/api/files/uploads/u_idle/parts/1", reader)
			if err != nil {
				t.Fatal(err)
			}
			request.ContentLength = 8
			type result struct {
				response *http.Response
				err      error
			}
			done := make(chan result, 1)
			go func() { response, err := client.Do(request); done <- result{response, err} }()
			if _, err = writer.Write([]byte("a")); err != nil {
				t.Fatal(err)
			}
			if stalled {
				select {
				case err = <-readResult:
					var timeout net.Error
					if !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatalf("stalled body did not time out: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("stalled handler kept its receiver indefinitely")
				}
				_ = writer.CloseWithError(io.ErrUnexpectedEOF)
			} else {
				// The total body duration exceeds idle, while every gap stays below it.
				for range 7 {
					time.Sleep(idle / 3)
					if _, err = writer.Write([]byte("b")); err != nil {
						t.Fatal(err)
					}
				}
				_ = writer.Close()
				select {
				case err = <-readResult:
					if err != nil {
						t.Fatalf("progressing part was timed out: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("completed body did not return")
				}
			}
			select {
			case got := <-done:
				if got.response != nil {
					_ = got.response.Body.Close()
				}
				if !stalled && (got.err != nil || got.response.StatusCode != 204) {
					t.Fatalf("progressing response: %+v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP client did not finish")
			}
			if !stalled {
				time.Sleep(idle + 50*time.Millisecond)
				response, err := client.Get(server.URL + "/control")
				if err != nil {
					t.Fatal("deadline leaked to next request:", err)
				}
				_ = response.Body.Close()
				if response.StatusCode != 204 || connections.Load() != 1 {
					t.Fatalf("keepalive connection lost after body deadline: status%d connections%d", response.StatusCode, connections.Load())
				}
			}
		})
	}
}
