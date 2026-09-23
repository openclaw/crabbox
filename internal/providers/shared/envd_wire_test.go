package shared

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"

	"github.com/openclaw/crabbox/internal/testutil"
)

func TestEncodeConnectJSONEnvelope(t *testing.T) {
	got, err := encodeConnectJSONEnvelope(map[string]string{"command": "ok"})
	want := append([]byte{0, 0, 0, 0, 16}, []byte(`{"command":"ok"}`)...)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("frame=%q err=%v, want %q", got, err, want)
	}
	_, err = encodeConnectJSONEnvelope(make(chan int))
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("marshal error=%v", err)
	}
}

func TestEnvdProcessStreamDelivery(t *testing.T) {
	for _, code := range []int{0, 7, -1, 137} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("code=%d/chunked=%t", code, chunked), func(t *testing.T) {
				body := bytes.Join([][]byte{
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "aGU=", "stderr": "d2E="}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "bGxv"}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": "cm4="}}}),
					envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}}),
					envdTestEnvelope(t, 2, map[string]any{}),
				}, nil)
				var reader io.Reader = bytes.NewReader(body)
				if chunked {
					reader = iotest.OneByteReader(reader)
				}
				var stdout, stderr bytes.Buffer
				endCalls := 0
				got, err := ParseEnvdProcessStream("fixture", reader, &stdout, &stderr, func(end EnvdProcessEnd, _ io.Writer, _ ...string) (int, error) {
					endCalls++
					if end != (EnvdProcessEnd{ExitCode: code, Exited: true}) {
						t.Fatalf("end=%+v", end)
					}
					return end.ExitCode, nil
				})
				if got != code || err != nil || endCalls != 1 || stdout.String() != "hello" || stderr.String() != "warn" {
					t.Fatalf("code=%d err=%v ends=%d stdout=%q stderr=%q", got, err, endCalls, stdout.String(), stderr.String())
				}
			})
		}
	}
}

func TestEnvdProcessStreamNormalEndAndLaterFailure(t *testing.T) {
	readFailure := errors.New("fixture read failed")
	for _, code := range []int{0, 7, -1, 137} {
		for _, ending := range []string{"EOF", "RPC failure", "read failure", "truncated"} {
			t.Run(fmt.Sprintf("code=%d/%s", code, ending), func(t *testing.T) {
				body := envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}})
				if ending == "RPC failure" {
					body = append(body, envdTestEnvelope(t, 2, map[string]any{"error": map[string]any{"code": "internal", "message": "fixture RPC failure"}})...)
				}
				if ending == "truncated" {
					body = append(body, 0)
				}
				var reader io.Reader = bytes.NewReader(body)
				if ending == "read failure" {
					reader = io.MultiReader(reader, iotest.ErrReader(readFailure))
				}
				got, err := ParseEnvdProcessStream("fixture", reader, io.Discard, io.Discard, envdTestEndCode)
				if ending == "EOF" {
					if got != code || err != nil {
						t.Fatalf("clean EOF code=%d err=%v", got, err)
					}
					return
				}
				if got != 1 || err == nil {
					t.Fatalf("later failure lost precedence: code=%d err=%v", got, err)
				}
				if ending == "RPC failure" && !strings.Contains(err.Error(), "fixture RPC failure") {
					t.Fatalf("RPC error=%v", err)
				}
				if ending == "read failure" && !errors.Is(err, readFailure) {
					t.Fatalf("read error=%v", err)
				}
				if ending == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated frame error=%v", err)
				}
			})
		}
	}
}

func TestEnvdProcessStreamWriterFailure(t *testing.T) {
	writeFailure := errors.New("fixture output write failed")
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			body := append(envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{stream: "b3V0cHV0"}}}), envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exited": true}}})...)
			var stdout, stderr io.Writer = io.Discard, io.Discard
			if stream == "stdout" {
				stdout = envdWireFailureWriter{writeFailure}
			} else {
				stderr = envdWireFailureWriter{writeFailure}
			}
			endCalled := false
			code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), stdout, stderr, func(EnvdProcessEnd, io.Writer, ...string) (int, error) { endCalled = true; return 0, nil })
			if code != 1 || !errors.Is(err, writeFailure) || endCalled {
				t.Fatalf("code=%d err=%v end=%t", code, err, endCalled)
			}
		})
	}
}

func TestEnvdProcessStreamRequiresProcessEnd(t *testing.T) {
	for _, trailer := range []bool{false, true} {
		t.Run(fmt.Sprintf("RPC trailer=%t", trailer), func(t *testing.T) {
			body := envdTestEnvelope(t, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": "cGFydGlhbA=="}}})
			if trailer {
				body = append(body, envdTestEnvelope(t, 2, map[string]any{})...)
			}
			var stdout bytes.Buffer
			code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), &stdout, io.Discard, envdTestEndCode)
			if code != 1 || err == nil || err.Error() != "fixture process stream ended without end event" || stdout.String() != "partial" {
				t.Fatalf("code=%d err=%v stdout=%q", code, err, stdout.String())
			}
		})
	}
}

func TestEnvdProcessStreamRedactsRPCError(t *testing.T) {
	const secret = "envd-stream-secret"
	body := envdTestEnvelope(t, 2, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "Bearer " + secret + " quota exceeded"}})
	code, err := ParseEnvdProcessStream("fixture", bytes.NewReader(body), io.Discard, io.Discard, envdTestEndCode, secret)
	if code != 1 || err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func envdTestEnvelope(t *testing.T, flags byte, value any) []byte {
	t.Helper()
	frame, err := encodeConnectJSONEnvelope(value)
	if err != nil {
		t.Fatal(err)
	}
	frame[0] = flags
	return frame
}

func envdTestEndCode(end EnvdProcessEnd, _ io.Writer, _ ...string) (int, error) {
	return end.ExitCode, nil
}

type envdWireFailureWriter struct{ err error }

func (w envdWireFailureWriter) Write([]byte) (int, error) { return 0, w.err }

// Controlled finite reads make upload completion observable without requiring
// cancellation to interrupt arbitrary borrowed readers.
type envdMultipartObservedReader struct {
	entered, release    chan struct{}
	once                sync.Once
	content             io.Reader
	failure             error
	returned            atomic.Bool
	accessedAfterReturn atomic.Bool
	closeCalls          atomic.Int32
}

func (r *envdMultipartObservedReader) Close() error { r.closeCalls.Add(1); return nil }

func (r *envdMultipartObservedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	if r.returned.Load() {
		r.accessedAfterReturn.Store(true)
	}
	if r.failure != nil {
		return 0, r.failure
	}
	return r.content.Read(p)
}

func TestEnvdMultipartCompletionAndErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                                     string
		status                                                   int
		sourceFailure, transportFailure, responseFailure, cancel bool
		want                                                     string
	}{
		{name: "early 2xx waits for producer", status: 200},
		{name: "source failure after early 2xx is returned", status: 200, sourceFailure: true, want: "source"},
		{name: "HTTP rejection remains primary", status: 403, sourceFailure: true, want: "http"},
		{name: "transport failure remains primary", transportFailure: true, sourceFailure: true, want: "transport"},
		{name: "cancellation waits for producer", cancel: true, want: "cancel"},
		{name: "success response read failure remains ignored", status: 200, responseFailure: true},
		{name: "error response read failure does not override status", status: 403, responseFailure: true, want: "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sourceErr := errors.New("synthetic source error: synthetic-session-token")
				transportErr := errors.New("synthetic transport error")
				responseErr := errors.New("synthetic response read error")
				httpErr := errors.New("synthetic provider HTTP error")
				source := &envdMultipartObservedReader{entered: make(chan struct{}), release: make(chan struct{}), content: strings.NewReader("finite payload")}
				if tc.sourceFailure {
					source.failure = sourceErr
				}
				var releaseOnce sync.Once
				resume := func() { releaseOnce.Do(func() { close(source.release) }) }
				defer resume()
				bodyDone := make(chan struct{}, 1)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
					go func() { _, _ = io.Copy(io.Discard, req.Body); _ = req.Body.Close(); bodyDone <- struct{}{} }()
					<-source.entered
					if tc.cancel {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					if tc.transportFailure {
						return nil, transportErr
					}
					var body io.Reader = strings.NewReader("ordinary response")
					if tc.responseFailure {
						body = iotest.ErrReader(responseErr)
					}
					return &http.Response{StatusCode: tc.status, Status: fmt.Sprintf("%d", tc.status), Header: make(http.Header), Body: io.NopCloser(body)}, nil
				})}
				result := make(chan error, 1)
				go func() {
					err := UploadEnvdFile(ctx, EnvdUploadFileRequest{Endpoint: "http://127.0.0.1:9443/files", TargetPath: "/tmp/archive.tgz", Content: source, HTTPClient: client, AccessToken: "synthetic-session-token",
						SetHeaders: func(*http.Request) {}, SummarizeError: func(b []byte) string { return string(b) }, APIError: func(code int, _ string, body string) error {
							if code != 403 {
								t.Errorf("API status=%d", code)
							}
							if tc.responseFailure && body != "" {
								t.Errorf("unexpected error response summary=%q", body)
							}
							return httpErr
						}})
					source.returned.Store(true)
					result <- err
				}()
				select {
				case <-source.entered:
				case err := <-result:
					t.Fatalf("returned before controlled read: %v", err)
				}
				if tc.cancel {
					cancel()
				}
				synctest.Wait()
				returnedEarly := source.returned.Load()
				resume()
				got := <-result
				<-bodyDone
				synctest.Wait()
				if source.closeCalls.Load() != 0 {
					t.Fatal("borrowed source was closed")
				}
				if returnedEarly || source.accessedAfterReturn.Load() {
					t.Fatal("producer retained source after return")
				}
				switch tc.want {
				case "":
					if got != nil {
						t.Fatalf("current success error=%v", got)
					}
				case "source":
					if !errors.Is(got, sourceErr) || strings.Contains(got.Error(), "synthetic-session-token") {
						t.Fatalf("source failure/redaction changed: %v", got)
					}
				case "http":
					if got != httpErr {
						t.Fatalf("HTTP error replaced: %v", got)
					}
				case "transport":
					if !errors.Is(got, transportErr) {
						t.Fatalf("transport error replaced: %v", got)
					}
				case "cancel":
					if !errors.Is(got, context.Canceled) {
						t.Fatalf("cancel error replaced: %v", got)
					}
				}
				t.Logf("observed earlyReturn=%v sourceAccessAfterReturn=%v sourceFailure=%v resultPolicy=%s", returnedEarly, source.accessedAfterReturn.Load(), tc.sourceFailure, tc.want)
			})
		})
	}
}

func TestMultipartFileExchangePanicJoinsWithoutReplacingPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &envdMultipartObservedReader{entered: make(chan struct{}), release: make(chan struct{}), content: strings.NewReader("payload")}
		var once sync.Once
		resume := func() { once.Do(func() { close(source.release) }) }
		defer resume()
		bodyDone := make(chan struct{}, 1)
		recovered := make(chan any, 1)
		marker := errors.New("exchange panic marker")
		go func() {
			defer func() { source.returned.Store(true); recovered <- recover() }()
			_ = WithMultipartFile(t.Context(), "archive", source, func(body io.ReadCloser, _ string) error {
				go func() { _, _ = io.Copy(io.Discard, body); _ = body.Close(); bodyDone <- struct{}{} }()
				<-source.entered
				panic(marker)
			}, func(err error) error { return err })
		}()
		<-source.entered
		synctest.Wait()
		early := false
		var got any
		select {
		case got = <-recovered:
			early = true
		default:
		}
		resume()
		if !early {
			got = <-recovered
		}
		<-bodyDone
		synctest.Wait()
		if early || got != marker || source.accessedAfterReturn.Load() || source.closeCalls.Load() != 0 {
			t.Fatalf("panic custody/policy changed: early=%v panic=%v", early, got)
		}
	})
}

func TestMultipartFileProducerPolicyAndFinalization(t *testing.T) {
	t.Run("mapper required before exchange", func(t *testing.T) {
		called := false
		err := WithMultipartFile(t.Context(), "archive", strings.NewReader(""), func(io.ReadCloser, string) error { called = true; return nil }, nil)
		if err == nil || called {
			t.Fatal("missing producer error policy admitted")
		}
	})
	t.Run("nil mapping cannot hide or expose failure", func(t *testing.T) {
		err := WithMultipartFile(t.Context(), "archive", iotest.ErrReader(errors.New("private-source-detail")), func(body io.ReadCloser, _ string) error {
			_, _ = io.Copy(io.Discard, body)
			_ = body.Close()
			return nil
		}, func(error) error { return nil })
		if err == nil || strings.Contains(err.Error(), "private-source-detail") {
			t.Fatalf("unsafe missing mapping result: %v", err)
		}
	})
	t.Run("finalization failure is mapped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &envdMultipartObservedReader{entered: make(chan struct{}), release: make(chan struct{}), content: strings.NewReader("")}
			close(source.release)
			mapped := errors.New("safe finalization error")
			calls := 0
			err := WithMultipartFile(t.Context(), "archive", source, func(body io.ReadCloser, _ string) error {
				_, readErr := body.Read(make([]byte, 4096))
				if readErr != nil {
					return readErr
				}
				<-source.entered
				_ = body.Close()
				return nil
			}, func(err error) error {
				calls++
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("finalization error=%v", err)
				}
				return mapped
			})
			if err != mapped || calls != 1 || source.closeCalls.Load() != 0 {
				t.Fatalf("finalization policy=%v calls=%d", err, calls)
			}
		})
	})
}

func TestMultipartFileRetainsSingleFileWireShape(t *testing.T) {
	payload := "archive\x00payload"
	mapped := func(err error) error { return err }
	err := WithMultipartFile(t.Context(), "archive name.tgz", strings.NewReader(payload), func(body io.ReadCloser, contentType string) error {
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return err
		}
		reader := multipart.NewReader(body, params["boundary"])
		part, err := reader.NextPart()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return err
		}
		if part.FormName() != "file" || part.FileName() != "archive name.tgz" || string(data) != payload {
			t.Error("multipart file shape changed")
		}
		if _, err := reader.NextPart(); err != io.EOF {
			t.Errorf("unexpected next part: %v", err)
		}
		return body.Close()
	}, mapped)
	if err != nil {
		t.Fatal(err)
	}
}

type envdCustodyReader struct {
	file             *os.File
	entered, release chan struct{}
	once             sync.Once
	returned         atomic.Bool
	readAfterReturn  atomic.Bool
	readErr          chan error
}

func (r *envdCustodyReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	if r.returned.Load() {
		r.readAfterReturn.Store(true)
	}
	n, err := r.file.Read(p)
	if err != nil {
		select {
		case r.readErr <- err:
		default:
		}
	}
	return n, err
}

func TestUploadEnvdFileReturnsBorrowedReaderBeforeCallerClosesArchive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "archive.tgz")
		if err := os.WriteFile(name, []byte("finite archive payload"), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		source := &envdCustodyReader{file: file, entered: make(chan struct{}), release: make(chan struct{}), readErr: make(chan error, 1)}
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(source.release) }) }
		defer resume()
		bodyDone := make(chan struct{}, 1)
		primary := errors.New("synthetic HTTP rejection")
		client := &http.Client{Transport: testutil.RoundTripFunc(func(req *http.Request) (*http.Response, error) {
			// A conforming transport may close the request body after returning headers.
			go func() { _, _ = io.Copy(io.Discard, req.Body); _ = req.Body.Close(); bodyDone <- struct{}{} }()
			<-source.entered
			return &http.Response{StatusCode: 403, Status: "403 Forbidden", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rejected"))}, nil
		})}
		result := make(chan error, 1)
		go func() {
			err := UploadEnvdFile(context.Background(), EnvdUploadFileRequest{
				Endpoint: "http://127.0.0.1:9443/files", TargetPath: "/tmp/archive.tgz", Content: source, HTTPClient: client,
				SetHeaders: func(*http.Request) {}, SummarizeError: func(b []byte) string { return string(b) },
				APIError: func(int, string, string) error { return primary },
			})
			// ArchiveWorkspace owns closing its prepared file after Upload returns an error.
			source.returned.Store(true)
			_ = file.Close()
			result <- err
		}()
		select {
		case <-source.entered:
		case err := <-result:
			t.Fatalf("returned before controlled read: %v", err)
		}
		synctest.Wait()
		returnedWhileReadActive := source.returned.Load()
		resume()
		got := <-result
		<-bodyDone
		synctest.Wait()
		if got != primary {
			t.Fatalf("HTTP error precedence changed: %v", got)
		}
		if returnedWhileReadActive || source.readAfterReturn.Load() {
			var readErr error
			select {
			case readErr = <-source.readErr:
			default:
			}
			t.Fatalf("upload returned custody while producer still read caller-closed archive: earlyReturn=%v readAfterReturn=%v sourceError=%v; HTTP error preserved", returnedWhileReadActive, source.readAfterReturn.Load(), readErr)
		}
	})
}
