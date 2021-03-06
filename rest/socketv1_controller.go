package rest

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jhillyerd/inbucket/httpd"
	"github.com/jhillyerd/inbucket/log"
	"github.com/jhillyerd/inbucket/msghub"
	"github.com/jhillyerd/inbucket/rest/model"
	"github.com/jhillyerd/inbucket/smtpd"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

// options for gorilla connection upgrader
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// msgListener handles messages from the msghub
type msgListener struct {
	hub     *msghub.Hub         // Global message hub
	c       chan msghub.Message // Queue of messages from Receive()
	mailbox string              // Name of mailbox to monitor, "" == all mailboxes
}

// newMsgListener creates a listener and registers it.  Optional mailbox parameter will restrict
// messages sent to WebSocket to that mailbox only.
func newMsgListener(hub *msghub.Hub, mailbox string) *msgListener {
	ml := &msgListener{
		hub:     hub,
		c:       make(chan msghub.Message, 100),
		mailbox: mailbox,
	}
	hub.AddListener(ml)
	return ml
}

// Receive handles an incoming message
func (ml *msgListener) Receive(msg msghub.Message) error {
	if ml.mailbox != "" && ml.mailbox != msg.Mailbox {
		// Did not match mailbox name
		return nil
	}
	ml.c <- msg
	return nil
}

// WSReader makes sure the websocket client is still connected, discards any messages from client
func (ml *msgListener) WSReader(conn *websocket.Conn) {
	defer ml.Close()
	conn.SetReadLimit(maxMessageSize)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		log.Tracef("HTTP[%v] Got WebSocket pong", conn.RemoteAddr())
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				// Unexpected close code
				log.Warnf("HTTP[%v] WebSocket error: %v", conn.RemoteAddr(), err)
			} else {
				log.Tracef("HTTP[%v] Closing WebSocket", conn.RemoteAddr())
			}
			break
		}
	}
}

// WSWriter makes sure the websocket client is still connected
func (ml *msgListener) WSWriter(conn *websocket.Conn) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		ml.Close()
	}()

	// Handle messages from hub until msgListener is closed
	for {
		select {
		case msg, ok := <-ml.c:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// msgListener closed, exit
				conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			header := &model.JSONMessageHeaderV1{
				Mailbox: msg.Mailbox,
				ID:      msg.ID,
				From:    msg.From,
				To:      msg.To,
				Subject: msg.Subject,
				Date:    msg.Date,
				Size:    msg.Size,
			}
			if conn.WriteJSON(header) != nil {
				// Write failed
				return
			}
		case <-ticker.C:
			// Send ping
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if conn.WriteMessage(websocket.PingMessage, []byte{}) != nil {
				// Write error
				return
			}
			log.Tracef("HTTP[%v] Sent WebSocket ping", conn.RemoteAddr())
		}
	}
}

// Close removes the listener registration
func (ml *msgListener) Close() {
	select {
	case <-ml.c:
		// Already closed
	default:
		ml.hub.RemoveListener(ml)
		close(ml.c)
	}
}

func MonitorAllMessagesV1(
	w http.ResponseWriter, req *http.Request, ctx *httpd.Context) (err error) {
	// Upgrade to Websocket
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return err
	}
	httpd.ExpWebSocketConnectsCurrent.Add(1)
	defer func() {
		_ = conn.Close()
		httpd.ExpWebSocketConnectsCurrent.Add(-1)
	}()

	log.Tracef("HTTP[%v] Upgraded to websocket", req.RemoteAddr)

	// Create, register listener; then interact with conn
	ml := newMsgListener(ctx.MsgHub, "")
	go ml.WSWriter(conn)
	ml.WSReader(conn)

	return nil
}

func MonitorMailboxMessagesV1(
	w http.ResponseWriter, req *http.Request, ctx *httpd.Context) (err error) {
	name, err := smtpd.ParseMailboxName(ctx.Vars["name"])
	if err != nil {
		return err
	}
	// Upgrade to Websocket
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return err
	}
	httpd.ExpWebSocketConnectsCurrent.Add(1)
	defer func() {
		_ = conn.Close()
		httpd.ExpWebSocketConnectsCurrent.Add(-1)
	}()

	log.Tracef("HTTP[%v] Upgraded to websocket", req.RemoteAddr)

	// Create, register listener; then interact with conn
	ml := newMsgListener(ctx.MsgHub, name)
	go ml.WSWriter(conn)
	ml.WSReader(conn)

	return nil
}
