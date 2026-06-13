package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const coordinatorControlDialTimeout = 1500 * time.Millisecond
const coordinatorControlRunEventLimit = 100

type coordinatorControlConn struct {
	conn *websocket.Conn
}

type coordinatorControlMessage struct {
	Type               string                `json:"type"`
	Protocol           int                   `json:"protocol,omitempty"`
	ClientID           string                `json:"clientID,omitempty"`
	RunID              string                `json:"runID,omitempty"`
	Events             []CoordinatorRunEvent `json:"events,omitempty"`
	NextSeq            int                   `json:"nextSeq,omitempty"`
	LeaseID            string                `json:"leaseID,omitempty"`
	OK                 bool                  `json:"ok,omitempty"`
	ExpiresAt          string                `json:"expiresAt,omitempty"`
	Code               string                `json:"code,omitempty"`
	Message            string                `json:"message,omitempty"`
	Error              string                `json:"error,omitempty"`
	IdleTimeoutSeconds int                   `json:"idleTimeoutSeconds,omitempty"`
	Telemetry          *LeaseTelemetry       `json:"telemetry,omitempty"`
}

func dialCoordinatorControl(ctx context.Context, coord *CoordinatorClient) (*coordinatorControlConn, error) {
	endpoint, err := coordinatorControlURL(coord.BaseURL)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	if err := coord.addRequestHeaders(ctx, headers); err != nil {
		return nil, err
	}
	opts := &websocket.DialOptions{
		HTTPHeader: headers,
	}
	if coord.Client != nil {
		opts.HTTPClient = coord.Client
	}
	dialCtx, cancel := context.WithTimeout(ctx, coordinatorControlDialTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, endpoint, opts)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	return &coordinatorControlConn{conn: conn}, nil
}

func coordinatorControlURL(baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch base.Scheme {
	case "http":
		base.Scheme = "ws"
	case "https":
		base.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported coordinator scheme %q", base.Scheme)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/control"
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func (c *coordinatorControlConn) close() {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *coordinatorControlConn) write(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *coordinatorControlConn) read(ctx context.Context) (coordinatorControlMessage, error) {
	typ, data, err := c.conn.Read(ctx)
	if err != nil {
		return coordinatorControlMessage{}, err
	}
	if typ != websocket.MessageText {
		return coordinatorControlMessage{}, fmt.Errorf("control websocket sent non-text frame")
	}
	var msg coordinatorControlMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return coordinatorControlMessage{}, err
	}
	return msg, nil
}

func followRunControlWebSocket(ctx context.Context, coord *CoordinatorClient, runID string, after int, poll time.Duration, stdout, stderr io.Writer) (int, bool, bool, error) {
	control, err := dialCoordinatorControl(ctx, coord)
	if err != nil {
		if ctx.Err() != nil {
			return after, false, false, ctx.Err()
		}
		return after, false, false, nil
	}
	defer control.close()
	nextAfter := after
	if !subscribeRunControl(ctx, control, runID, nextAfter) {
		return nextAfter, false, true, nil
	}
	for {
		readCtx, readCancel := context.WithTimeout(ctx, poll)
		msg, err := control.read(readCtx)
		readCancel()
		if err != nil {
			if ctx.Err() != nil {
				return nextAfter, false, true, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				done, err := coordinatorRunDone(ctx, coord, runID)
				if err != nil {
					return nextAfter, false, true, err
				}
				if done {
					return nextAfter, true, true, nil
				}
				continue
			}
			done, doneErr := coordinatorRunDone(ctx, coord, runID)
			if doneErr != nil {
				return nextAfter, false, true, doneErr
			}
			if done {
				return nextAfter, true, true, nil
			}
			return nextAfter, false, true, nil
		}
		switch msg.Type {
		case "hello", "pong", "heartbeat":
			continue
		case "error":
			return nextAfter, false, true, nil
		case "run_events":
			for _, event := range msg.Events {
				if event.Seq <= nextAfter {
					continue
				}
				nextAfter = event.Seq
				printAttachEvent(stdout, stderr, event)
			}
			ackCtx, ackCancel := context.WithTimeout(ctx, 2*time.Second)
			_ = control.write(ackCtx, map[string]any{
				"type":  "ack",
				"runID": runID,
				"seq":   nextAfter,
			})
			ackCancel()
			if len(msg.Events) >= coordinatorControlRunEventLimit {
				if !subscribeRunControl(ctx, control, runID, nextAfter) {
					return nextAfter, false, true, nil
				}
			}
		}
	}
}

func subscribeRunControl(ctx context.Context, control *coordinatorControlConn, runID string, after int) bool {
	writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
	err := control.write(writeCtx, map[string]any{
		"type":  "subscribe_run",
		"runID": runID,
		"after": after,
		"limit": coordinatorControlRunEventLimit,
	})
	writeCancel()
	return err == nil
}

func coordinatorRunDone(ctx context.Context, coord *CoordinatorClient, runID string) (bool, error) {
	run, err := coord.Run(ctx, runID)
	if err != nil {
		return false, err
	}
	return run.State != "running", nil
}

func (c *coordinatorControlConn) heartbeat(ctx context.Context, leaseID string, idleTimeout *time.Duration, telemetry *LeaseTelemetry) error {
	payload := coordinatorControlMessage{
		Type:      "heartbeat",
		LeaseID:   leaseID,
		Telemetry: telemetry,
	}
	if idleTimeout != nil && *idleTimeout > 0 {
		payload.IdleTimeoutSeconds = int(idleTimeout.Seconds())
	}
	if err := c.write(ctx, payload); err != nil {
		return err
	}
	for {
		msg, err := c.read(ctx)
		if err != nil {
			return err
		}
		if msg.Type != "heartbeat" {
			continue
		}
		if !msg.OK {
			if msg.Error != "" {
				return fmt.Errorf("control heartbeat failed: %s", msg.Error)
			}
			return fmt.Errorf("control heartbeat failed")
		}
		return nil
	}
}
