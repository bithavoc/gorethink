package gorethink

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dancannon/gorethink/types"

	p "github.com/dancannon/gorethink/ql2"
)

const (
	respHeaderLen = 12
)

// Connection is a connection to a rethinkdb database. Connection is not thread
// safe and should only be accessed be a single goroutine
type Connection struct {
	address string
	opts    *ConnectOpts
	conn    net.Conn

	_       [4]byte
	mu      sync.Mutex
	token   int64
	cursors map[int64]*Cursor
	bad     bool
}

// NewConnection creates a new connection to the database server
func NewConnection(address string, opts *ConnectOpts) (*Connection, error) {
	var err error
	c := &Connection{
		address: address,
		opts:    opts,
		cursors: make(map[int64]*Cursor),
	}
	// Connect to Server
	nd := net.Dialer{Timeout: c.opts.Timeout}
	if c.opts.TLSConfig == nil {
		c.conn, err = nd.Dial("tcp", address)
	} else {
		c.conn, err = tls.DialWithDialer(&nd, "tcp", address, c.opts.TLSConfig)
	}
	if err != nil {
		return nil, err
	}
	// Enable TCP Keepalives on TCP connections
	if tc, ok := c.conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAlive(true); err != nil {
			// Don't send COM_QUIT before handshake.
			c.conn.Close()
			c.conn = nil
			return nil, err
		}
	}
	// Send handshake request
	if err = c.writeHandshakeReq(); err != nil {
		c.Close()
		return nil, err
	}
	// Read handshake response
	err = c.readHandshakeSuccess()
	if err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// Close closes the underlying net.Conn
func (c *Connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	c.cursors = nil

	return nil
}

// Query sends a Query to the database, returning both the raw Response and a
// Cursor which should be used to view the query's response.
//
// This function is used internally by Run which should be used for most queries.
func (c *Connection) Query(q Query) (*types.Response, *Cursor, error) {
	c.mu.Lock()
	if c == nil {
		c.mu.Unlock()
		return nil, nil, ErrConnectionClosed
	}
	if c.conn == nil {
		c.bad = true
		c.mu.Unlock()
		return nil, nil, ErrConnectionClosed
	}

	// Add token if query is a START/NOREPLY_WAIT
	if q.Type == p.Query_START || q.Type == p.Query_NOREPLY_WAIT {
		q.Token = c.nextToken()
		if c.opts.Database != "" {
			var err error
			q.Opts["db"], err = DB(c.opts.Database).build()
			if err != nil {
				return nil, nil, err
			}
		}
	}
	c.mu.Unlock()

	err := c.sendQuery(q)
	if err != nil {
		return nil, nil, err
	}

	if noreply, ok := q.Opts["noreply"]; ok && noreply.(bool) {
		return nil, nil, nil
	}

	for {
		response, err := c.readResponse()
		if err != nil {
			return nil, nil, err
		}

		if response.Token == q.Token {
			// If this was the requested response process and return
			return c.processResponse(q, response)
		} else if _, ok := c.cursors[response.Token]; ok {
			// If the token is in the cursor cache then process the response
			c.processResponse(q, response)
		} else {
			putResponse(response)
		}
	}
}

// sendQuery marshals the Query and sends the JSON to the server.
func (c *Connection) sendQuery(q Query) error {
	// Build query
	b, err := json.Marshal(q.build())
	if err != nil {
		return RQLDriverError{"Error building query"}
	}

	// Set timeout
	if c.opts.WriteTimeout == 0 {
		c.conn.SetWriteDeadline(time.Time{})
	} else {
		c.conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
	}

	// Send the JSON encoding of the query itself.
	if err = c.writeQuery(q.Token, b); err != nil {
		c.bad = true
		return RQLConnectionError{err.Error()}
	}

	return nil
}

// getToken generates the next query token, used to number requests and match
// responses with requests.
func (c *Connection) nextToken() int64 {
	// requires c.token to be 64-bit aligned on ARM
	return atomic.AddInt64(&c.token, 1)
}

// readResponse attempts to read a Response from the server, if no response
// could be read then an error is returned.
func (c *Connection) readResponse() (*types.Response, error) {
	// Set timeout
	if c.opts.ReadTimeout == 0 {
		c.conn.SetReadDeadline(time.Time{})
	} else {
		c.conn.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout))
	}

	// Read response header (token+length)
	headerBuf := [respHeaderLen]byte{}
	if _, err := c.read(headerBuf[:], respHeaderLen); err != nil {
		return nil, err
	}

	responseToken := int64(binary.LittleEndian.Uint64(headerBuf[:8]))
	messageLength := binary.LittleEndian.Uint32(headerBuf[8:])

	// Read the JSON encoding of the Response itself.
	b := make([]byte, int(messageLength))

	if _, err := c.read(b, int(messageLength)); err != nil {
		c.bad = true
		return nil, RQLConnectionError{err.Error()}
	}

	// Decode the response
	var response = newCachedResponse()
	if err := basicDecode(b, response); err != nil {
		c.bad = true
		return nil, RQLDriverError{err.Error()}
	}
	response.Token = responseToken

	return response, nil
}

func (c *Connection) processResponse(q Query, response *types.Response) (*types.Response, *Cursor, error) {
	switch response.Type {
	case p.Response_CLIENT_ERROR:
		return c.processErrorResponse(q, response, RQLClientError{rqlResponseError{response, q.Term}})
	case p.Response_COMPILE_ERROR:
		return c.processErrorResponse(q, response, RQLCompileError{rqlResponseError{response, q.Term}})
	case p.Response_RUNTIME_ERROR:
		return c.processErrorResponse(q, response, RQLRuntimeError{rqlResponseError{response, q.Term}})
	case p.Response_SUCCESS_ATOM:
		return c.processAtomResponse(q, response)
	case p.Response_SUCCESS_PARTIAL:
		return c.processPartialResponse(q, response)
	case p.Response_SUCCESS_SEQUENCE:
		return c.processSequenceResponse(q, response)
	case p.Response_WAIT_COMPLETE:
		return c.processWaitResponse(q, response)
	default:
		putResponse(response)
		return nil, nil, RQLDriverError{"Unexpected response type"}
	}
}

func (c *Connection) processErrorResponse(q Query, response *types.Response, err error) (*types.Response, *Cursor, error) {
	c.mu.Lock()
	cursor := c.cursors[response.Token]

	delete(c.cursors, response.Token)
	c.mu.Unlock()

	return response, cursor, err
}

func (c *Connection) processAtomResponse(q Query, response *types.Response) (*types.Response, *Cursor, error) {
	// Create cursor
	cursor := newCursor(c, "Cursor", response.Token, q.Term, q.Opts)
	cursor.profile = response.Profile

	cursor.extend(response)

	return response, cursor, nil
}

func (c *Connection) processPartialResponse(q Query, response *types.Response) (*types.Response, *Cursor, error) {
	cursorType := "Cursor"
	if len(response.Notes) > 0 {
		switch response.Notes[0] {
		case p.Response_SEQUENCE_FEED:
			cursorType = "Feed"
		case p.Response_ATOM_FEED:
			cursorType = "AtomFeed"
		case p.Response_ORDER_BY_LIMIT_FEED:
			cursorType = "OrderByLimitFeed"
		case p.Response_UNIONED_FEED:
			cursorType = "UnionedFeed"
		case p.Response_INCLUDES_STATES:
			cursorType = "IncludesFeed"
		}
	}

	c.mu.Lock()
	cursor, ok := c.cursors[response.Token]
	if !ok {
		// Create a new cursor if needed
		cursor = newCursor(c, cursorType, response.Token, q.Term, q.Opts)
		cursor.profile = response.Profile

		c.cursors[response.Token] = cursor
	}
	c.mu.Unlock()

	cursor.extend(response)

	return response, cursor, nil
}

func (c *Connection) processSequenceResponse(q Query, response *types.Response) (*types.Response, *Cursor, error) {
	c.mu.Lock()
	cursor, ok := c.cursors[response.Token]
	if !ok {
		// Create a new cursor if needed
		cursor = newCursor(c, "Cursor", response.Token, q.Term, q.Opts)
		cursor.profile = response.Profile
	}

	delete(c.cursors, response.Token)
	c.mu.Unlock()

	cursor.extend(response)

	return response, cursor, nil
}

func (c *Connection) processWaitResponse(q Query, response *types.Response) (*types.Response, *Cursor, error) {
	c.mu.Lock()
	delete(c.cursors, response.Token)
	c.mu.Unlock()

	return response, nil, nil
}

var responseCache = make(chan *types.Response, 16)

func newCachedResponse() *types.Response {
	select {
	case r := <-responseCache:
		return r
	default:
		return new(types.Response)
	}
}

func putResponse(r *types.Response) {
	*r = types.Response{} // zero it
	select {
	case responseCache <- r:
	default:
	}
}
