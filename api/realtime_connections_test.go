package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func TestRealtimeConnectionOwnerClosesHijackedHandlerBeforeRuntimeRetirement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := NewRealtimeConnectionOwner()
	handler := NewHandler(nil, nil)
	handler.RealtimeConnections = owner
	router := gin.New()
	finished := make(chan struct{})
	router.GET("/realtime", func(c *gin.Context) {
		c.Set("site_name", "shutdown-test")
		handler.HandleSystemRealtime(c)
		close(finished)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/realtime", nil)
	if err != nil {
		t.Fatal("dial realtime websocket:", err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal("read initial realtime message:", err)
	}

	owner.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("realtime handler remained pinned after Engine closed hijacked connection")
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("client websocket remained open after process shutdown")
	}
}

func TestRealtimeConnectionOwnerClosesStreamsRegisteredAfterShutdown(t *testing.T) {
	owner := NewRealtimeConnectionOwner()
	owner.Close()
	closed := make(chan struct{})
	owner.Register(func() { close(closed) })()
	select {
	case <-closed:
	default:
		t.Fatal("stream registered after shutdown was not closed")
	}
}
