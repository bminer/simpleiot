// Package respreader provides a convenient way to read data from devices that use
// prompt/response protocols such as Modbus (and other RS485 protocols) and modem
// AT commands. The fundamental assumption is a device takes some variable amount of
// time to respond to a request, formats up a packet, and then streams it out the
// serial port. Once the response data starts streaming, and significant gap with
// no data indicates the response is complete.
package respreader

import (
	"errors"
	"io"
	"time"
)

// ErrorTimeout indicates the reader timed out
var ErrorTimeout = errors.New("timeout")

// ResponseReader is used for prompt/response communication protocols where a prompt
// is sent, and some time later a response is received. Typically, the target takes
// some amount to formulate the response, and then streams it out. There are two delays:
// an overall timeout, and then an intercharacter timeout that is activated once the
// first byte is received. The thought is that once you received the 1st byte, all the
// data should stream out continously and a short timeout can be used to determine the
// end of the packet.
type ResponseReader struct {
	reader       io.Reader
	timeout      time.Duration
	chunkTimeout time.Duration
	size         int
	dataChan     chan []byte
}

// NewResponseReader creates a new response reader
func NewResponseReader(reader io.Reader, timeout time.Duration, chunkTimeout time.Duration) *ResponseReader {
	rr := ResponseReader{
		reader:       reader,
		timeout:      timeout,
		chunkTimeout: chunkTimeout,
		size:         128,
		dataChan:     make(chan []byte),
	}
	// we have to start a reader goroutine here that lives for the life
	// of the reader because there is no
	// way to stop a blocked goroutine
	go rr.readInput()
	return &rr
}

func (rr *ResponseReader) Read(buffer []byte) (int, error) {
	if len(buffer) <= 0 {
		return 0, errors.New("must supply non-zero length buffer")
	}

	timeout := time.NewTimer(rr.timeout)
	count := 0

	for {
		select {
		case newData, ok := <-rr.dataChan:
			// copy data from chan buffer to Read() buf
			for i := 0; count < len(buffer) && i < len(newData); i++ {
				buffer[count] = newData[i]
				count++
			}

			if !ok {
				return count, io.EOF
			}

			timeout.Reset(rr.chunkTimeout)

		case <-timeout.C:
			if count > 0 {
				return count, nil
			}

			return count, ErrorTimeout

		}
	}
}

// Flush is used to flush any input data
func (rr *ResponseReader) Flush() (int, error) {
	timeout := time.NewTimer(rr.chunkTimeout)
	count := 0

	for {
		select {
		case newData, ok := <-rr.dataChan:
			count += len(newData)
			if !ok {
				return count, io.EOF
			}

			timeout.Reset(rr.chunkTimeout)

		case <-timeout.C:
			return count, nil
		}
	}
}

// readInput is used by a goroutine to read data from the underlying io.Reader
func (rr *ResponseReader) readInput() {
	for {
		tmp := make([]byte, rr.size)
		length, err := rr.reader.Read(tmp)
		if err != nil {
			break
		}
		tmp = tmp[0:length]
		rr.dataChan <- tmp
	}
	close(rr.dataChan)
}

// ResponseReadWriter is a convenience type that implements io.ReadWriter. Write
// calls flush reader before writing the prompt.
type ResponseReadWriter struct {
	writer io.Writer
	reader *ResponseReader
}

// NewResponseReadWriter creates a new response reader
func NewResponseReadWriter(iorw io.ReadWriter, timeout time.Duration, chunkTimeout time.Duration) *ResponseReadWriter {
	return &ResponseReadWriter{
		writer: iorw,
		reader: NewResponseReader(iorw, timeout, chunkTimeout),
	}
}

// Read reads with timouts
func (rrw *ResponseReadWriter) Read(buffer []byte) (int, error) {
	return rrw.reader.Read(buffer)
}

// Write flushes all data from reader, and then passes through write call.
func (rrw *ResponseReadWriter) Write(buffer []byte) (int, error) {
	n, err := rrw.reader.Flush()
	if err != nil {
		return n, err
	}

	return rrw.writer.Write(buffer)
}

// ResponseReadWriteCloser is a convenience type that implements io.ReadWriteCloser.
// Write calls flush reader before writing the prompt.
type ResponseReadWriteCloser struct {
	closer io.Closer
	writer io.Writer
	reader *ResponseReader
}

// NewResponseReadWriteCloser creates a new response reader
func NewResponseReadWriteCloser(iorw io.ReadWriteCloser, timeout time.Duration, chunkTimeout time.Duration) *ResponseReadWriteCloser {
	return &ResponseReadWriteCloser{
		closer: iorw,
		writer: iorw,
		reader: NewResponseReader(iorw, timeout, chunkTimeout),
	}
}

// Read reads with timouts
func (rrwc *ResponseReadWriteCloser) Read(buffer []byte) (int, error) {
	return rrwc.reader.Read(buffer)
}

// Write flushes all data from reader, and then passes through write call.
func (rrwc *ResponseReadWriteCloser) Write(buffer []byte) (int, error) {
	n, err := rrwc.reader.Flush()
	if err != nil {
		return n, err
	}

	return rrwc.writer.Write(buffer)
}

// Close is a passthrough call.
func (rrwc *ResponseReadWriteCloser) Close() error {
	return rrwc.closer.Close()
}