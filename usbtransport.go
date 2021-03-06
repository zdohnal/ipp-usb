/* ipp-usb - HTTP reverse proxy, backed by IPP-over-USB connection to device
 *
 * Copyright (C) 2020 and up by Alexander Pevzner (pzz@apevzner.com)
 * See LICENSE for license terms and conditions
 *
 * USB transport for HTTP
 */

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"sync/atomic"
	"time"
)

// UsbTransport implements HTTP transport functionality over USB
type UsbTransport struct {
	addr         UsbAddr       // Device address
	info         UsbDeviceInfo // USB device info
	log          *Logger       // Device's own logger
	dev          *UsbDevHandle // Underlying USB device
	connPool     chan *usbConn // Pool of idle connections
	connList     []*usbConn    // List of all connections
	connReleased chan struct{} // Signalled when connection released
	shutdown     chan struct{} // Closed by Shutdown()
	connstate    *usbConnState // Connections state tracker
	quirks       [][2]string   // HTTP header quirks
}

// NewUsbTransport creates new http.RoundTripper backed by IPP-over-USB
func NewUsbTransport(desc UsbDeviceDesc) (*UsbTransport, error) {
	// Open the device
	dev, err := UsbOpenDevice(desc)
	if err != nil {
		return nil, err
	}

	// Create UsbTransport
	transport := &UsbTransport{
		addr:         desc.UsbAddr,
		log:          NewLogger(),
		dev:          dev,
		connPool:     make(chan *usbConn, len(desc.IfAddrs)),
		connReleased: make(chan struct{}),
		shutdown:     make(chan struct{}),
		connstate:    newUsbConnState(len(desc.IfAddrs)),
	}

	// Obtain device info
	transport.info, err = dev.UsbDeviceInfo()
	if err != nil {
		dev.Close()
		return nil, err
	}

	transport.log.Cc(Console)
	transport.log.ToDevFile(transport.info)
	transport.log.SetLevels(Conf.LogDevice)

	// Setup quirks
	transport.makeQuirks()

	// Write device info to the log
	log := transport.log.Begin().
		Nl(LogDebug).
		Debug(' ', "===============================").
		Info('+', "%s: added %s", transport.addr, transport.info.ProductName).
		Debug(' ', "Device info:").
		Debug(' ', "  Ident:         %s", transport.info.Ident()).
		Debug(' ', "  Manufacturer:  %s", transport.info.Manufacturer).
		Debug(' ', "  Product:       %s", transport.info.ProductName).
		Debug(' ', "  MfgAndProduct: %s", transport.info.MfgAndProduct).
		Nl(LogDebug)

	log.Debug(' ', "Device quirks:")
	for _, quirk := range transport.quirks {
		log.Debug(' ', "  %s: %s", quirk[0], quirk[1])
	}
	log.Nl(LogDebug)

	transport.dumpUSBparams(log)
	log.Nl(LogDebug)

	log.Debug(' ', "USB interfaces:")
	log.Debug(' ', "  Config Interface Alt Class SubClass Proto")
	for _, ifdesc := range desc.IfDescs {
		prefix := byte(' ')
		if ifdesc.IsIppOverUsb() {
			prefix = '*'
		}

		log.Debug(prefix,
			"     %-3d     %-3d    %-3d %-3d    %-3d     %-3d",
			ifdesc.Config, ifdesc.IfNum,
			ifdesc.Alt, ifdesc.Class, ifdesc.SubClass, ifdesc.Proto)
	}
	log.Nl(LogDebug)
	log.Commit()

	// Open connections
	for i, ifaddr := range desc.IfAddrs {
		var conn *usbConn
		conn, err = transport.openUsbConn(i, ifaddr)
		if err != nil {
			goto ERROR
		}
		transport.connPool <- conn
		transport.connList = append(transport.connList, conn)
	}

	return transport, nil

	// Error: cleanup and exit
ERROR:
	for _, conn := range transport.connList {
		conn.destroy()
	}

	dev.Close()
	return nil, err
}

// Dump USB stack parameters to the UsbTransport's log
func (transport *UsbTransport) dumpUSBparams(log *LogMessage) {
	const usbParamsDir = "/sys/module/usbcore/parameters"

	// Obtain list of parameter names (file names)
	dir, err := os.Open(usbParamsDir)
	if err != nil {
		return
	}

	files, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return
	}

	sort.Strings(files)
	if len(files) == 0 {
		return
	}

	// Compute max width of parameter names
	wid := 0
	for _, file := range files {
		if wid < len(file) {
			wid = len(file)
		}
	}

	wid++

	// Write the table
	log.Debug(' ', "USB stack parameters")

	for _, file := range files {
		p, _ := ioutil.ReadFile(usbParamsDir + "/" + file)
		if p == nil {
			p = []byte("-")
		} else {
			p = bytes.TrimSpace(p)
		}

		log.Debug(' ', "  %*s  %s", -wid, file+":", p)
	}
}

// Get count of connections still in use
func (transport *UsbTransport) connInUse() int {
	return cap(transport.connPool) - len(transport.connPool)
}

// Shutdown gracefully shuts down the transport. If provided
// context expires before shutdown completion, Shutdown
// returns the Context's error
func (transport *UsbTransport) Shutdown(ctx context.Context) error {
	close(transport.shutdown)

	for {
		n := transport.connInUse()
		if n == 0 {
			break
		}

		transport.log.Info('-', "%s: shutdown: %d connections still in use",
			transport.addr, n)

		select {
		case <-transport.connReleased:
		case <-ctx.Done():
			transport.log.Error('-', "%s: %s: shutdown timeout expired",
				transport.addr, transport.info.ProductName)
			return ctx.Err()
		}
	}

	return nil
}

// Close the transport
func (transport *UsbTransport) Close() {
	if transport.connInUse() > 0 {
		transport.log.Info('-', "%s: resetting %s",
			transport.addr, transport.info.ProductName)
		transport.dev.Reset()
	}

	for _, conn := range transport.connList {
		conn.destroy()
	}

	transport.dev.Close()
	transport.log.Info('-', "%s: removed %s",
		transport.addr, transport.info.ProductName)
}

// Log returns device's own logger
func (transport *UsbTransport) Log() *Logger {
	return transport.log
}

// UsbDeviceInfo returns USB device information for the device
// behind the transport
func (transport *UsbTransport) UsbDeviceInfo() UsbDeviceInfo {
	return transport.info
}

// RoundTrip implements http.RoundTripper interface
func (transport *UsbTransport) RoundTrip(r *http.Request) (
	*http.Response, error) {
	session := int(atomic.AddInt32(&httpSessionID, 1)-1) % 1000

	return transport.RoundTripWithSession(session, r)
}

// RoundTripWithSession executes a single HTTP transaction, returning
// a Response for the provided Request. Session number, for logging,
// provided as a separate parameter
func (transport *UsbTransport) RoundTripWithSession(session int,
	rq *http.Request) (*http.Response, error) {

	// Log the request
	transport.log.HTTPRqParams(LogDebug, '>', session, rq)

	// Prevent request from being canceled from outside
	// We cannot do it on USB: closing USB connection
	// doesn't drain buffered data that server is
	// about to send to client
	outreq := rq.WithContext(context.Background())
	outreq.Cancel = nil

	// Remove Expect: 100-continue, if any
	outreq.Header.Del("Expect")

	// Apply quirks
	for _, quirk := range transport.quirks {
		if quirk[1] != "" {
			outreq.Header.Set(quirk[0], quirk[1])
		} else {
			outreq.Header.Del(quirk[0])
		}
	}

	// Don't let Go's stdlib to add Connection: close header
	// automatically
	outreq.Close = false

	// Add User-Agent, if missed. It is just cosmetic
	if _, found := outreq.Header["User-Agent"]; !found {
		outreq.Header["User-Agent"] = []string{"ipp-usb"}
	}

	// Wrap request body
	if outreq.Body != nil {
		outreq.Body = &usbRequestBodyWrapper{
			log:     transport.log,
			session: session,
			body:    outreq.Body,
		}
	}

	// Prepare to correctly handle HTTP transaction, in a case
	// client drops request in a middle of reading body
	switch {
	case outreq.ContentLength <= 0:
		// Nothing to do

	case outreq.ContentLength < 16384:
		// Body is small, prefetch it before sending to USB
		buf := &bytes.Buffer{}
		_, err := io.CopyN(buf, outreq.Body, outreq.ContentLength)
		if err != nil {
			return nil, err
		}

		outreq.Body.Close()
		outreq.Body = ioutil.NopCloser(buf)

		transport.log.HTTPDebug('>', session,
			"body is small (%d bytes), prefetched before sending",
			buf.Len())

	default:
		// Force chunked encoding, so if client drops request,
		// we still be able to correctly handle HTTP transaction
		transport.log.HTTPDebug('>', session,
			"body is large (%d bytes), sending as chunked",
			outreq.ContentLength)

		outreq.ContentLength = -1
	}

	// Log request details
	transport.log.Begin().
		HTTPRequest(LogTraceHTTP, '>', session, outreq).
		Commit()

	// Allocate USB connection
	conn, err := transport.usbConnGet(rq.Context())
	if err != nil {
		return nil, err
	}

	transport.log.HTTPDebug(' ', session, "connection %d allocated", conn.index)

	// Send request and receive a response
	err = outreq.Write(conn)
	if err != nil {
		transport.log.HTTPError('!', session, "%s", err)
		conn.put()
		return nil, err
	}

	resp, err := http.ReadResponse(conn.reader, outreq)
	if err != nil {
		transport.log.HTTPError('!', session, "%s", err)
		conn.put()
		return nil, err
	}

	// Wrap response body
	resp.Body = &usbResponseBodyWrapper{
		log:     transport.log,
		session: session,
		body:    resp.Body,
		conn:    conn,
	}

	// Log the response
	if resp != nil {
		transport.log.Begin().
			HTTPRspStatus(LogDebug, '<', session, outreq, resp).
			HTTPResponse(LogTraceHTTP, '<', session, resp).
			Commit()
	}

	return resp, nil
}

// makeQuirks computes device-specific quirks that applied
// to outgoing HTTP request header
//
// Each quirk is a touple of two strings: header name
// and header value. If header value is "", the corresponding
// header field is deleted from request
//
// For now it affects connection keep-alive settings.
//
// Although setting connection keep-alive for HTTP requests
// going to USB sounds meaningless, without it some printers
// sometimes stuck in generating HTTP response, so effectively
// blocking the USB interface. And the "good" keep-alive mode
// is different for different devices!
//
// It's a pure black magic, but we have to live with it
func (transport *UsbTransport) makeQuirks() {
	switch transport.info.MfgAndProduct {
	case "HP OfficeJet Pro 8730":
		transport.quirks = [][2]string{{"Connection", "close"}}

	case "HP LaserJet MFP M28-M31":
		transport.quirks = [][2]string{{"Connection", "keep-alive"}}
	}

	transport.quirks = [][2]string{{"Connection", ""}}
}

// usbRequestBodyWrapper wraps http.Request.Body, adding
// data path instrumentation
type usbRequestBodyWrapper struct {
	log     *Logger       // Device's logger
	session int           // HTTP session, for logging
	count   int           // Total count of received bytes
	body    io.ReadCloser // Request.body
	drained bool          // EOF or error has been seen
}

// Read from usbRequestBodyWrapper
func (wrap *usbRequestBodyWrapper) Read(buf []byte) (int, error) {
	n, err := wrap.body.Read(buf)
	wrap.count += n

	if err != nil {
		wrap.log.HTTPDebug('>', wrap.session,
			"request body: got %d bytes; %s", wrap.count, err)
		err = io.EOF
		wrap.drained = true
	}

	return n, err
}

// Close usbRequestBodyWrapper
func (wrap *usbRequestBodyWrapper) Close() error {
	if !wrap.drained {
		wrap.log.HTTPDebug('>', wrap.session,
			"request body: got %d bytes; closed", wrap.count)
	}

	return wrap.body.Close()
}

// usbResponseBodyWrapper wraps http.Response.Body and guarantees
// that connection will be always drained before closed
type usbResponseBodyWrapper struct {
	log     *Logger       // Device's logger
	session int           // HTTP session, for logging
	body    io.ReadCloser // Response.body
	conn    *usbConn      // Underlying USB connection
	count   int           // Total count of received bytes
	drained bool          // EOF or error has been seen
}

// Read from usbResponseBodyWrapper
func (wrap *usbResponseBodyWrapper) Read(buf []byte) (int, error) {
	n, err := wrap.body.Read(buf)
	wrap.count += n

	if err != nil {
		wrap.log.HTTPDebug('<', wrap.session,
			"response body: got %d bytes; %s", wrap.count, err)
		wrap.drained = true
	}
	return n, err
}

// Close usbResponseBodyWrapper
func (wrap *usbResponseBodyWrapper) Close() error {
	// If EOF or error seen, we can close synchronously
	if wrap.drained {
		wrap.body.Close()
		wrap.conn.put()
		return nil
	}

	// Otherwise, we need to drain USB connection
	wrap.log.HTTPDebug('<', wrap.session, "client has gone; draining response from USB")
	go func() {
		defer func() {
			v := recover()
			if v != nil {
				Log.Panic(v)
			}
		}()

		io.Copy(ioutil.Discard, wrap.body)
		wrap.body.Close()
		wrap.conn.put()
	}()

	return nil
}

// usbConn implements an USB connection
type usbConn struct {
	transport *UsbTransport // Transport that owns the connection
	index     int           // Connection index (for logging)
	iface     *UsbInterface // Underlying interface
	reader    *bufio.Reader // For http.ReadResponse
	cntRecv   int           // Total bytes received
	cntSent   int           // Total bytes sent
}

// Open usbConn
func (transport *UsbTransport) openUsbConn(
	index int, ifaddr UsbIfAddr) (*usbConn, error) {

	dev := transport.dev

	transport.log.Debug(' ', "USB[%d]: open: %s", index, ifaddr)

	// Initialize connection structure
	conn := &usbConn{
		transport: transport,
		index:     index,
	}

	conn.reader = bufio.NewReader(conn)

	// Obtain interface
	var err error
	conn.iface, err = dev.OpenUsbInterface(ifaddr)
	if err != nil {
		goto ERROR
	}

	return conn, nil

	// Error: cleanup and exit
ERROR:
	transport.log.Error('!', "USB[%d]: %s", index, err)
	if conn.iface != nil {
		conn.iface.Close()
	}

	return nil, err
}

// Read from USB
func (conn *usbConn) Read(b []byte) (int, error) {
	conn.transport.connstate.beginRead(conn)
	defer conn.transport.connstate.doneRead(conn)

	// Note, to avoid LIBUSB_TRANSFER_OVERFLOW errors
	// from libusb, input buffer size must always
	// be aligned by 512 bytes
	//
	// However if caller requests less that 512 bytes, we
	// can't align here simply by shrinking the buffer,
	// because it will result a zero-size buffer. At
	// this case we assume caller knows what it
	// doing (actually bufio never behaves this way)
	if n := len(b); n >= 512 {
		n &= ^511
		b = b[0:n]
	}

	backoff := time.Millisecond * 100
	for {
		n, err := conn.iface.Recv(b, 0)
		conn.cntRecv += n

		conn.transport.log.Add(LogTraceHTTP, '<',
			"USB[%d]: read: wanted %d got %d total %d",
			conn.index, len(b), n, conn.cntRecv)

		if err != nil {
			conn.transport.log.Error('!',
				"USB[%d]: recv: %s", conn.index, err)
		}

		if n != 0 || err != nil {
			return n, err
		}
		conn.transport.log.Error('!',
			"USB[%d]: zero-size read", conn.index)

		time.Sleep(backoff)
		backoff *= 2
		if backoff > time.Millisecond*1000 {
			backoff = time.Millisecond * 1000
		}
	}
}

// Write to USB
func (conn *usbConn) Write(b []byte) (int, error) {
	conn.transport.connstate.beginWrite(conn)
	defer conn.transport.connstate.doneWrite(conn)

	n, err := conn.iface.Send(b, 0)
	conn.cntSent += n

	conn.transport.log.Add(LogTraceHTTP, '>',
		"USB[%d]: write: wanted %d sent %d total %d",
		conn.index, len(b), n, conn.cntSent)

	if err != nil {
		conn.transport.log.Error('!',
			"USB[%d]: send: %s", conn.index, err)
	}

	return n, err
}

// Allocate a connection
func (transport *UsbTransport) usbConnGet(ctx context.Context) (*usbConn, error) {
	select {
	case <-transport.shutdown:
		return nil, ErrShutdown
	case <-ctx.Done():
		return nil, ctx.Err()
	case conn := <-transport.connPool:
		transport.connstate.gotConn(conn)
		transport.log.Debug(' ', "USB[%d]: connection allocated, %s",
			conn.index, transport.connstate)

		return conn, nil
	}
}

// Release the connection
func (conn *usbConn) put() {
	transport := conn.transport

	conn.reader.Reset(conn)
	conn.cntRecv = 0
	conn.cntSent = 0

	transport.connstate.putConn(conn)
	transport.log.Debug(' ', "USB[%d]: connection released, %s",
		conn.index, transport.connstate)

	transport.connPool <- conn

	select {
	case transport.connReleased <- struct{}{}:
	default:
	}
}

// Destroy USB connection
func (conn *usbConn) destroy() {
	conn.transport.log.Debug(' ', "USB[%d]: closed", conn.index)
	conn.iface.Close()
}

// usbConnState tracks connections state, for logging
type usbConnState struct {
	alloc []int32 // Per-connection "allocated" flag
	read  []int32 // Per-connection "reading" flag
	write []int32 // Per-connection "writing" flag
}

// newUsbConnState creates a new usbConnState for given
// number of connections
func newUsbConnState(cnt int) *usbConnState {
	return &usbConnState{
		alloc: make([]int32, cnt),
		read:  make([]int32, cnt),
		write: make([]int32, cnt),
	}
}

// gotConn notifies usbConnState, that connection is allocated
func (state *usbConnState) gotConn(conn *usbConn) {
	atomic.AddInt32(&state.alloc[conn.index], 1)
}

// putConn notifies usbConnState, that connection is released
func (state *usbConnState) putConn(conn *usbConn) {
	atomic.AddInt32(&state.alloc[conn.index], -1)
}

// beginRead notifies usbConnState, that read is started
func (state *usbConnState) beginRead(conn *usbConn) {
	atomic.AddInt32(&state.read[conn.index], 1)
}

// doneRead notifies usbConnState, that read is done
func (state *usbConnState) doneRead(conn *usbConn) {
	atomic.AddInt32(&state.read[conn.index], -1)
}

// beginWrite notifies usbConnState, that write is started
func (state *usbConnState) beginWrite(conn *usbConn) {
	atomic.AddInt32(&state.write[conn.index], 1)
}

// doneWrite notifies usbConnState, that write is done
func (state *usbConnState) doneWrite(conn *usbConn) {
	atomic.AddInt32(&state.write[conn.index], -1)
}

// String returns a string, representing connections state
func (state *usbConnState) String() string {
	buf := make([]byte, 0, 64)
	used := 0

	for i := range state.alloc {
		a := atomic.LoadInt32(&state.alloc[i])
		r := atomic.LoadInt32(&state.read[i])
		w := atomic.LoadInt32(&state.write[i])

		if len(buf) != 0 {
			buf = append(buf, ' ')
		}

		if a|r|w == 0 {
			buf = append(buf, '-', '-', '-')
		} else {
			used++

			if a != 0 {
				buf = append(buf, 'a')
			} else {
				buf = append(buf, '-')
			}

			if r != 0 {
				buf = append(buf, 'r')
			} else {
				buf = append(buf, '-')
			}

			if w != 0 {
				buf = append(buf, 'w')
			} else {
				buf = append(buf, '-')
			}
		}
	}

	return fmt.Sprintf("%d in use: %s", used, buf)
}
