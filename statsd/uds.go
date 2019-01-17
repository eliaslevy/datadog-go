package statsd

import (
	"fmt"
	"net"
	"time"
)

/*
UDSTimeout holds the default timeout for UDS socket writes, as they can get
blocking when the receiving buffer is full.
*/
const defaultUDSTimeout = 1 * time.Millisecond

// udsWriter is an internal class wrapping around management of UDS connection
type udsWriter struct {
	// Address to send metrics to, needed to allow reconnection on error
	addr net.Addr
	// Established connection object, or nil if not connected yet
	conn net.Conn
	// write timeout
	writeTimeout time.Duration

	droppedDatagrams int
	datagramQueue    chan []byte
	stopChan         chan struct{}
}

// New returns a pointer to a new udsWriter given a socket file path as addr.
func newUdsWriter(addr string) (*udsWriter, error) {
	udsAddr, err := net.ResolveUnixAddr("unixgram", addr)
	if err != nil {
		return nil, err
	}

	writer := &udsWriter{
		addr:          udsAddr,
		conn:          nil,
		writeTimeout:  defaultUDSTimeout,
		datagramQueue: make(chan []byte, 8192),
		stopChan:      make(chan struct{}),
	}
	go writer.sendLoop()
	return writer, nil
}

func (w *udsWriter) sendLoop() {
	for {
		select {
		case datagram := <-w.datagramQueue:
			for {
				_, err := w.write(datagram)
				if err == nil {
					break
				}
			}
		case <-w.stopChan:
			return
		}
	}
}

// SetWriteTimeout allows the user to set a custom write timeout
func (w *udsWriter) SetWriteTimeout(d time.Duration) error {
	w.writeTimeout = d
	return nil
}

// Write data to the UDS connection with write timeout and minimal error handling:
// create the connection if nil, and destroy it if the statsd server has disconnected
func (w *udsWriter) Write(data []byte) (int, error) {
	select {
	case w.datagramQueue <- data:
		return len(data), nil
	default:
		w.droppedDatagrams++
		return 0, fmt.Errorf("uds datagram queue is full (the agent might not be able to keep up)")
	}
}

func (w *udsWriter) write(data []byte) (int, error) {
	conn, err := w.ensureConnection()
	if err != nil {
		return 0, err
	}

	w.conn.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	n, err := conn.Write(data)

	if e, isNetworkErr := err.(net.Error); !isNetworkErr || !e.Temporary() {
		// err is not temporary, Statsd server disconnected, retry connecting at next packet
		w.unsetConnection()
		return 0, e
	}

	return n, err
}

func (w *udsWriter) Close() error {
	close(w.stopChan)
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

func (w *udsWriter) ensureConnection() (net.Conn, error) {
	// Check if we've already got a socket we can use
	currentConn := w.conn

	if currentConn != nil {
		return currentConn, nil
	}

	// Looks like we might need to connect - try again with write locking.
	if w.conn != nil {
		return w.conn, nil
	}

	newConn, err := net.Dial(w.addr.Network(), w.addr.String())
	if err != nil {
		return nil, err
	}
	w.conn = newConn
	return newConn, nil
}

func (w *udsWriter) unsetConnection() {
	w.conn = nil
}
