package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"

	"kami/ngrok/common"
)

const (
	defaultServerAddr  = "103.78.0.204:8881"
	defaultLocalHost   = "localhost"
	defaultLocalPort   = 80
	heartbeatInterval  = 20 * time.Second
	backendIdleTimeout = 5 * time.Second
	backendIdleRetries = 3
	udpControlInterval = 3 * time.Second
	udpControlTimeout  = 6 * time.Second
)

const debugUDP = false

const (
	udpMsgHandshake byte = 1
	udpMsgData      byte = 2
	udpMsgClose     byte = 3
	udpMsgPing      byte = 4
	udpMsgPong      byte = 5
)

type client struct {
	serverAddr string
	localAddr  string
	key        string
	clientID   string
	remotePort int
	publicHost string
	protocol   string

	control     net.Conn
	enc         *jsonWriter
	dec         *jsonReader
	closeOnce   sync.Once
	done        chan struct{}
	trafficQuit chan struct{}
	statusCh    chan trafficStats
	bytesUp     uint64
	bytesDown   uint64
	pingCh      chan time.Duration
	pingSent    int64
	pingMs      int64
	uiEnabled   bool
	tui         *terminalUI
	exitFlag    uint32

	udpMu       sync.Mutex
	udpSessions map[string]*udpClientSession
	udpConn     *net.UDPConn
	udpReady    bool

	udpCtrlMu        sync.Mutex
	udpPingTicker    *time.Ticker
	udpPingStop      chan struct{}
	udpLastPing      time.Time
	udpLastPong      time.Time
	udpControlWarned bool
	udpCtrlStatus    string

	dataMu           sync.Mutex
	lastServerData   time.Time
	lastBackendData  time.Time
	totalUDPSessions uint64
}

type trafficStats struct {
	upRate    string
	downRate  string
	totalUp   string
	totalDown string
}

type terminalUI struct {
	screen tcell.Screen
	mutex  sync.Mutex
	quit   chan struct{}
	once   sync.Once
}

type udpClientSession struct {
	id         string
	conn       *net.UDPConn
	remoteAddr string
	closeOnce  sync.Once
	closed     chan struct{}
	timer      *time.Timer
	idleCount  int
}

func (s *udpClientSession) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.conn != nil {
			s.conn.Close()
		}
	})
}

func newTerminalUI() (*terminalUI, error) {
	s, err := tcell.NewScreen()
	if err != nil {
		return nil, err
	}
	if err := s.Init(); err != nil {
		s.Fini()
		return nil, err
	}
	s.Clear()
	s.ShowCursor(-1, -1)
	ui := &terminalUI{
		screen: s,
		quit:   make(chan struct{}),
	}
	go ui.pollEvents()
	return ui, nil
}

func (t *terminalUI) Close() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		close(t.quit)
	})
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.screen.Fini()
}

func (t *terminalUI) QuitChan() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.quit
}

func (t *terminalUI) pollEvents() {
	for {
		ev := t.screen.PollEvent()
		switch tev := ev.(type) {
		case *tcell.EventKey:
			if tev.Key() == tcell.KeyCtrlC || tev.Key() == tcell.KeyEscape || tev.Rune() == 'q' || tev.Rune() == 'Q' {
				t.once.Do(func() { close(t.quit) })
				return
			}
		case *tcell.EventResize:
			t.mutex.Lock()
			t.screen.Sync()
			t.mutex.Unlock()
		}
	}
}

func (t *terminalUI) Render(lines []string) {
	if t == nil {
		return
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	width, height := t.screen.Size()
	t.screen.Clear()

	innerWidth := 0
	for _, line := range lines {
		if w := len([]rune(line)); w > innerWidth {
			innerWidth = w
		}
	}
	medWidth := width - 4
	if medWidth > innerWidth {
		innerWidth = medWidth
	}
	frameWidth := innerWidth + 4
	startX := 0
	if width > frameWidth {
		startX = (width - frameWidth) / 2
	}

	startY := 0
	frameHeight := len(lines) + 2
	if height > frameHeight {
		startY = (height - frameHeight) / 2
	}

	borderRunes := []rune("+" + strings.Repeat("=", innerWidth+2) + "+")
	drawRunes(t.screen, startX, startY, borderRunes)
	for i, line := range lines {
		row := startY + i + 1
		content := "| " + line + strings.Repeat(" ", innerWidth-len([]rune(line))) + " |"
		drawRunes(t.screen, startX, row, []rune(content))
	}
	drawRunes(t.screen, startX, startY+frameHeight-1, borderRunes)
	for y := 0; y < height; y++ {
		t.screen.SetContent(width-1, y, ' ', nil, tcell.StyleDefault)
	}
	t.screen.Show()
}

func drawRunes(screen tcell.Screen, x, y int, runes []rune) {
	style := tcell.StyleDefault
	for i, r := range runes {
		screen.SetContent(x+i, y, r, nil, style)
	}
}

type jsonWriter struct {
	enc *json.Encoder
	mu  sync.Mutex
}

type jsonReader struct {
	dec *json.Decoder
	mu  sync.Mutex
}

func (w *jsonWriter) Encode(msg common.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(msg)
}

func (r *jsonReader) Decode(msg *common.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dec.Decode(msg)
}

func main() {
	serverAddr := flag.String("server", defaultServerAddr, "địa chỉ server (ip:port)")
	hostFlag := flag.String("host", defaultLocalHost, "dịch tới host nội bộ (mặc định 127.0.0.1)")
	portFlag := flag.Int("port", defaultLocalPort, "dịch tới port nội bộ (bị ghi đè nếu truyền đối số)")
	clientID := flag.String("id", "", "định danh client (ví dụ: my-tunnel)")
	protoFlag := flag.String("proto", "tcp", "giao thức tunnel (tcp hoặc udp)")
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	id := strings.TrimSpace(*clientID)
	if id == "" {
		host, _ := os.Hostname()
		id = fmt.Sprintf("client-%s", host)
	}

	localHost := strings.TrimSpace(*hostFlag)
	if localHost == "" {
		localHost = defaultLocalHost
	}
	localPort := *portFlag

	args := normalizedArgs(flag.Args())
	switch len(args) {
	case 0:
		// use flag defaults
	case 1:
		if p, err := strconv.Atoi(args[0]); err == nil && p > 0 && p <= 65535 {
			localPort = p
		} else {
			log.Fatalf("[client] port không hợp lệ: %q", args[0])
		}
	default:
		if strings.TrimSpace(args[0]) != "" {
			localHost = args[0]
		}
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 && p <= 65535 {
			localPort = p
		} else {
			log.Fatalf("[client] port không hợp lệ: %q", args[1])
		}
	}

	if localPort <= 0 || localPort > 65535 {
		log.Fatalf("[client] port không hợp lệ: %d", localPort)
	}

	proto := strings.ToLower(strings.TrimSpace(*protoFlag))
	if proto != "udp" {
		proto = "tcp"
	}

	cl := &client{
		serverAddr: *serverAddr,
		localAddr:  net.JoinHostPort(localHost, strconv.Itoa(localPort)),
		clientID:   id,
		protocol:   proto,
		uiEnabled:  term.IsTerminal(int(os.Stdout.Fd())),
	}

	if cl.uiEnabled {
		if tui, err := newTerminalUI(); err == nil {
			cl.tui = tui
		} else {
			log.Printf("[client] không khởi tạo UI tcell: %v", err)
			cl.uiEnabled = false
		}
	}
	defer cl.closeUI()

	if err := cl.run(); err != nil {
		log.Fatalf("[client] lỗi: %v", err)
	}
}

func (c *client) run() error {
	for {
		if err := c.connectControl(); err != nil {
			log.Printf("[client] kết nối control thất bại: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if err := c.receiveLoop(); err != nil {
			log.Printf("[client] control lỗi: %v", err)
		}
		c.closeControl()
		if atomic.LoadUint32(&c.exitFlag) == 1 {
			return nil
		}
		time.Sleep(3 * time.Second)
		log.Printf("[client] thử reconnect control...")
	}
}

func (c *client) connectControl() error {
	conn, err := net.Dial("tcp", c.serverAddr)
	if err != nil {
		return err
	}

	c.closeOnce = sync.Once{}
	c.done = make(chan struct{})
	c.trafficQuit = make(chan struct{})
	c.statusCh = make(chan trafficStats, 1)
	c.pingCh = make(chan time.Duration, 1)
	c.control = conn
	c.enc = &jsonWriter{enc: common.NewEncoder(conn)}
	c.dec = &jsonReader{dec: common.NewDecoder(bufio.NewReader(conn))}
	c.stopUDPPing()
	c.setUDPCtrlStatus("offline")
	atomic.StoreUint64(&c.bytesUp, 0)
	atomic.StoreUint64(&c.bytesDown, 0)
	atomic.StoreInt64(&c.pingSent, 0)
	atomic.StoreInt64(&c.pingMs, -1)
	select {
	case c.pingCh <- time.Duration(-1):
	default:
	}
	success := false
	defer func() {
		if !success {
			if c.control != nil {
				c.control.Close()
				c.control = nil
			}
			if c.trafficQuit != nil {
				close(c.trafficQuit)
				c.trafficQuit = nil
			}
			c.enc = nil
			c.dec = nil
			c.udpMu.Lock()
			if c.udpConn != nil {
				c.udpConn.Close()
				c.udpConn = nil
			}
			c.udpMu.Unlock()
			c.stopUDPPing()
		}
	}()

	register := common.Message{
		Type:     "register",
		Key:      c.key,
		ClientID: c.clientID,
		Target:   c.localAddr,
		Protocol: c.protocol,
	}
	if err := c.enc.Encode(register); err != nil {
		return err
	}

	resp := common.Message{}
	if err := c.dec.Decode(&resp); err != nil {
		return err
	}
	if resp.Type != "registered" {
		return fmt.Errorf("đăng ký thất bại: %+v", resp)
	}
	if strings.TrimSpace(resp.Key) != "" {
		c.key = strings.TrimSpace(resp.Key)
	}
	c.remotePort = resp.RemotePort
	if strings.TrimSpace(resp.Protocol) != "" {
		c.protocol = strings.ToLower(strings.TrimSpace(resp.Protocol))
	}
	hostPart := c.serverAddr
	if host, _, err := net.SplitHostPort(c.serverAddr); err == nil {
		hostPart = host
	}
	c.publicHost = net.JoinHostPort(hostPart, strconv.Itoa(c.remotePort))
	c.setUDPCtrlStatus("n/a")
	log.Printf("[client] đăng ký thành công, public port %d", c.remotePort)
	if c.protocol == "udp" {
		c.setUDPCtrlStatus("offline")
		if err := c.setupUDPChannel(); err != nil {
			log.Printf("[client] thiết lập UDP control lỗi: %v", err)
		} else if debugUDP {
			log.Printf("[client] UDP control đang chờ handshake với %s", c.serverAddr)
		}
	}
	go c.heartbeatLoop()
	go c.trafficLoop()
	go c.displayLoop()
	success = true
	return nil
}

func (c *client) receiveLoop() error {
	for {
		msg := common.Message{}
		if err := c.dec.Decode(&msg); err != nil {
			if isEOF(err) {
				return io.EOF
			}
			return err
		}

		switch msg.Type {
		case "proxy":
			go c.handleProxy(msg.ID)
		case "udp_open":
			c.handleUDPOpen(msg)
		case "udp_close":
			c.handleUDPClose(msg.ID)
		case "ping":
			_ = c.enc.Encode(common.Message{Type: "pong"})
		case "pong":
			c.recordPingReply()
		case "error":
			log.Printf("[client] server báo lỗi: %s", msg.Error)
		default:
			log.Printf("[client] thông điệp không hỗ trợ: %+v", msg)
		}
	}
}

func (c *client) heartbeatLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			start := time.Now()
			if err := c.enc.Encode(common.Message{Type: "ping"}); err != nil {
				return
			}
			atomic.StoreInt64(&c.pingSent, start.UnixNano())
		case <-c.done:
			return
		}
	}
}

func (c *client) trafficLoop() {
	const interval = 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastUp, lastDown uint64
	firstStats := trafficStats{
		upRate:    formatRate(0, interval),
		downRate:  formatRate(0, interval),
		totalUp:   formatBytes(0),
		totalDown: formatBytes(0),
	}
	select {
	case c.statusCh <- firstStats:
	default:
	}
	for {
		select {
		case <-ticker.C:
			up := atomic.LoadUint64(&c.bytesUp)
			down := atomic.LoadUint64(&c.bytesDown)
			upDelta := up - lastUp
			downDelta := down - lastDown
			lastUp = up
			lastDown = down
			stats := trafficStats{
				upRate:    formatRate(upDelta, interval),
				downRate:  formatRate(downDelta, interval),
				totalUp:   formatBytes(up),
				totalDown: formatBytes(down),
			}
			select {
			case c.statusCh <- stats:
			default:
				select {
				case <-c.statusCh:
				default:
				}
				c.statusCh <- stats
			}
		case <-c.trafficQuit:
			return
		case <-c.done:
			return
		}
	}
}

func (c *client) displayLoop() {
	if !c.uiEnabled {
		for {
			select {
			case stats, ok := <-c.statusCh:
				if ok {
					fmt.Printf("[ui] traffic ▲ %s/s ▼ %s/s ↑ %s ↓ %s\n", stats.upRate, stats.downRate, stats.totalUp, stats.totalDown)
				}
			case duration, ok := <-c.pingCh:
				if ok {
					fmt.Printf("[ui] ping %s\n", duration)
				}
			case <-c.done:
				return
			case <-c.trafficQuit:
				return
			}
		}
	}

	useANSI := c.tui == nil
	if useANSI {
		fmt.Print("\033[2J\033[H\033[?25l")
		defer fmt.Print("\033[?25h\033[2J\033[H")
	}

	traffic := trafficStats{
		upRate:    formatRate(0, time.Second),
		downRate:  formatRate(0, time.Second),
		totalUp:   formatBytes(0),
		totalDown: formatBytes(0),
	}
	ping := time.Duration(-1)
	hasTraffic := false

	uiQuit := c.tui.QuitChan()

	render := func() {
		if !hasTraffic {
			return
		}
		c.renderFrame(traffic, ping)
	}

	for {
		select {
		case stats, ok := <-c.statusCh:
			if !ok {
				return
			}
			traffic = stats
			hasTraffic = true
			render()
		case duration, ok := <-c.pingCh:
			if !ok {
				ping = time.Duration(-1)
				continue
			}
			ping = duration
			render()
		case <-c.done:
			return
		case <-c.trafficQuit:
			return
		case <-uiQuit:
			atomic.StoreUint32(&c.exitFlag, 1)
			c.closeControl()
			return
		}
	}
}

func (c *client) handleProxy(id string) {
	if c.protocol == "udp" {
		log.Printf("[client] bỏ qua proxy TCP vì tunnel đang ở chế độ UDP")
		return
	}
	if strings.TrimSpace(id) == "" {
		return
	}

	localConn, err := net.Dial("tcp", c.localAddr)
	if err != nil {
		log.Printf("[client] không kết nối được backend %s: %v", c.localAddr, err)
		c.reportProxyError(id, err)
		return
	}

	srvConn, err := net.Dial("tcp", c.serverAddr)
	if err != nil {
		log.Printf("[client] không connect server cho proxy: %v", err)
		localConn.Close()
		c.reportProxyError(id, err)
		return
	}

	enc := common.NewEncoder(srvConn)
	if err := enc.Encode(common.Message{
		Type:     "proxy",
		Key:      c.key,
		ClientID: c.clientID,
		ID:       id,
	}); err != nil {
		log.Printf("[client] gửi proxy handshake lỗi: %v", err)
		localConn.Close()
		srvConn.Close()
		return
	}

	go proxyCopyCount(srvConn, localConn, &c.bytesUp)
	go proxyCopyCount(localConn, srvConn, &c.bytesDown)
}

func (c *client) handleUDPOpen(msg common.Message) {
	if c.protocol != "udp" {
		return
	}
	if strings.TrimSpace(msg.ID) == "" {
		return
	}
	if msg.Protocol != "" && strings.ToLower(msg.Protocol) != "udp" {
		return
	}
	backend, err := c.resolveBackendUDP()
	if err != nil {
		log.Printf("[client] resolve backend UDP lỗi: %v", err)
		c.sendUDPClose(msg.ID)
		return
	}
	conn, err := net.DialUDP("udp", nil, backend)
	if err != nil {
		log.Printf("[client] không kết nối được backend UDP %s: %v", backend, err)
		c.sendUDPClose(msg.ID)
		return
	}
	sess := &udpClientSession{
		id:         msg.ID,
		conn:       conn,
		remoteAddr: strings.TrimSpace(msg.RemoteAddr),
		closed:     make(chan struct{}),
	}
	c.udpMu.Lock()
	if c.udpSessions == nil {
		c.udpSessions = make(map[string]*udpClientSession)
	}
	if old, ok := c.udpSessions[msg.ID]; ok {
		delete(c.udpSessions, msg.ID)
		old.Close()
	}
	c.udpSessions[msg.ID] = sess
	atomic.AddUint64(&c.totalUDPSessions, 1)
	c.udpMu.Unlock()
	go c.readFromUDPLocal(sess)
}

func (c *client) handleUDPClose(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	c.removeUDPSession(id, false)
}

func (c *client) setupUDPChannel() error {
	addr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(4 * 1024 * 1024)
	_ = conn.SetWriteBuffer(4 * 1024 * 1024)
	c.udpMu.Lock()
	if c.udpConn != nil {
		c.udpConn.Close()
	}
	c.udpConn = conn
	c.udpReady = false
	c.udpMu.Unlock()
	c.stopUDPPing()
	c.setUDPCtrlStatus("handshake")
	go c.readUDPControl(conn)
	for i := 0; i < 3; i++ {
		if err := c.sendUDPHandshake(); err != nil {
			log.Printf("[client] gửi UDP handshake burst #%d lỗi: %v", i+1, err)
		} else if debugUDP {
			log.Printf("[client] gửi UDP handshake burst #%d tới %s", i+1, addr)
		}
		if i < 2 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	go c.udpHandshakeRetry()
	return nil
}

func (c *client) readUDPControl(conn *net.UDPConn) {
	defer c.stopUDPPing()
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("[client] đọc UDP control lỗi: %v", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		packet := make([]byte, n)
		copy(packet, buf[:n])
		c.handleUDPControlPacket(packet)
	}
}

func (c *client) handleUDPControlPacket(packet []byte) {
	if len(packet) < 3 {
		return
	}
	msgType := packet[0]
	key, idx, ok := decodeUDPField(packet, 1)
	if !ok || key == "" || key != c.key {
		return
	}
	switch msgType {
	case udpMsgData:
		id, next, ok := decodeUDPField(packet, idx)
		if !ok || id == "" {
			return
		}
		payload := make([]byte, len(packet)-next)
		copy(payload, packet[next:])
		c.handleUDPDataPacket(id, payload)
	case udpMsgClose:
		id, _, ok := decodeUDPField(packet, idx)
		if !ok || id == "" {
			return
		}
		c.handleUDPClose(id)
	case udpMsgHandshake:
		c.udpMu.Lock()
		if !c.udpReady && debugUDP {
			log.Printf("[client] UDP control handshake thành công từ %s", c.serverAddr)
		}
		c.udpReady = true
		c.udpMu.Unlock()
		c.startUDPPing()
	case udpMsgPong:
		_, next, ok := decodeUDPField(packet, idx)
		if !ok {
			return
		}
		payload := make([]byte, len(packet)-next)
		copy(payload, packet[next:])
		c.handleUDPPong(payload)
	case udpMsgPing:
		_, next, ok := decodeUDPField(packet, idx)
		if !ok {
			return
		}
		payload := make([]byte, len(packet)-next)
		copy(payload, packet[next:])
		c.sendUDPPong(payload)
	default:
	}
}

func (c *client) handleUDPDataPacket(id string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	sess := c.getUDPSession(id)
	if sess == nil {
		return
	}
	c.markServerData()
	if _, err := sess.conn.Write(payload); err != nil {
		log.Printf("[client] ghi về backend UDP lỗi: %v", err)
		c.removeUDPSession(id, true)
		return
	}
	c.startBackendWait(id)
	if debugUDP {
		log.Printf("[client] nhận %d bytes UDP từ server cho phiên %s", len(payload), id)
	}
	atomic.AddUint64(&c.bytesDown, uint64(len(payload)))
}

func (c *client) readFromUDPLocal(sess *udpClientSession) {
	buf := make([]byte, 65535)
	for {
		n, err := sess.conn.Read(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("[client] đọc UDP backend lỗi: %v", err)
			}
			break
		}
		if n == 0 {
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		c.cancelBackendWait(sess.id)
		c.markBackendData()
		c.sendUDPData(sess.id, payload)
	}
	c.removeUDPSession(sess.id, true)
}

func (c *client) resolveBackendUDP() (*net.UDPAddr, error) {
	return net.ResolveUDPAddr("udp", c.localAddr)
}

func (c *client) getUDPSession(id string) *udpClientSession {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if c.udpSessions == nil {
		return nil
	}
	return c.udpSessions[id]
}

func (c *client) handleBackendTimeout(id string) {
	sess := c.getUDPSession(id)
	remote := ""
	if sess != nil {
		remote = sess.remoteAddr
	}
	if sess != nil {
		sess.idleCount++
		if sess.idleCount < backendIdleRetries {
			if debugUDP {
				log.Printf("[client] backend phiên %s (remote %s) chưa phản hồi (%d/%d)", id, remote, sess.idleCount, backendIdleRetries)
			}
			// restart timer
			c.startBackendWait(id)
			return
		}
	}
	log.Printf("[client] backend không phản hồi cho phiên %s (remote %s) - đóng phiên", id, remote)
	if c.enc != nil {
		_ = c.enc.Encode(common.Message{Type: "udp_idle", ID: id, Protocol: "udp"})
	}
	c.removeUDPSession(id, true)
}

func (c *client) removeUDPSession(id string, notify bool) {
	c.udpMu.Lock()
	sess := c.udpSessions[id]
	if sess != nil {
		delete(c.udpSessions, id)
	}
	c.udpMu.Unlock()
	if sess == nil {
		return
	}
	sess.Close()
	if notify {
		c.sendUDPClose(id)
	}
}

func (c *client) startBackendWait(id string) {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if sess, ok := c.udpSessions[id]; ok {
		if sess.timer != nil {
			sess.timer.Stop()
		}
		sess.idleCount = 0
		sess.timer = time.AfterFunc(backendIdleTimeout, func() {
			c.handleBackendTimeout(id)
		})
	}
}

func (c *client) cancelBackendWait(id string) {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if sess, ok := c.udpSessions[id]; ok && sess.timer != nil {
		sess.timer.Stop()
		sess.timer = nil
		sess.idleCount = 0
	}
}

func (c *client) markServerData() {
	c.dataMu.Lock()
	c.lastServerData = time.Now()
	c.dataMu.Unlock()
}

func (c *client) markBackendData() {
	c.dataMu.Lock()
	c.lastBackendData = time.Now()
	c.dataMu.Unlock()
}

func (c *client) getLastServerData() time.Time {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	return c.lastServerData
}

func (c *client) getLastBackendData() time.Time {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	return c.lastBackendData
}

func (c *client) closeAllUDPSessions() {
	c.udpMu.Lock()
	sessions := make([]*udpClientSession, 0, len(c.udpSessions))
	for _, sess := range c.udpSessions {
		if sess.timer != nil {
			sess.timer.Stop()
			sess.timer = nil
		}
		sessions = append(sessions, sess)
	}
	c.udpSessions = make(map[string]*udpClientSession)
	c.udpMu.Unlock()
	for _, sess := range sessions {
		sess.Close()
	}
}

func (c *client) sendUDPData(id string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	if err := c.writeUDP(udpMsgData, id, payload); err != nil {
		log.Printf("[client] gửi udp_data lỗi: %v", err)
		return
	}
	atomic.AddUint64(&c.bytesUp, uint64(len(payload)))
}

func (c *client) sendUDPClose(id string) {
	if err := c.writeUDP(udpMsgClose, id, nil); err != nil {
		log.Printf("[client] gửi udp_close lỗi: %v", err)
	}
	if c.enc != nil {
		_ = c.enc.Encode(common.Message{Type: "udp_close", ID: id, Protocol: "udp"})
	}
}

func (c *client) sendUDPHandshake() error {
	return c.writeUDP(udpMsgHandshake, "", nil)
}

func (c *client) sendUDPPing(payload []byte) error {
	return c.writeUDP(udpMsgPing, "", payload)
}

func (c *client) sendUDPPong(payload []byte) {
	if err := c.writeUDP(udpMsgPong, "", payload); err != nil && debugUDP {
		log.Printf("[client] gửi udp_pong lỗi: %v", err)
	}
}

func (c *client) udpHandshakeRetry() {
	const (
		retryInterval    = 500 * time.Millisecond
		handshakeTimeout = 10 * time.Second
		maxRetries       = 20
	)

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()

	attempts := 0
	for {
		c.udpMu.Lock()
		ready := c.udpReady
		connPresent := c.udpConn != nil
		c.udpMu.Unlock()
		if ready || !connPresent {
			if attempts > 0 && ready {
				log.Printf("[client] UDP handshake thành công sau %d lần thử (%d ms)", attempts+1, attempts*int(retryInterval/time.Millisecond))
			}
			return
		}
		select {
		case <-ticker.C:
			attempts++
			if attempts > maxRetries {
				log.Printf("[client] UDP handshake thất bại sau %d lần thử", maxRetries)
				c.udpMu.Lock()
				if c.udpConn != nil {
					c.udpConn.Close()
					c.udpConn = nil
				}
				c.udpMu.Unlock()
				c.setUDPCtrlStatus("offline")
				return
			}
			if err := c.sendUDPHandshake(); err != nil {
				if debugUDP {
					log.Printf("[client] retry handshake #%d lỗi: %v", attempts, err)
				}
			} else if debugUDP {
				log.Printf("[client] retry handshake #%d/%d", attempts, maxRetries)
			}
		case <-timeout.C:
			log.Printf("[client] UDP handshake timeout sau %v", handshakeTimeout)
			c.udpMu.Lock()
			if c.udpConn != nil {
				c.udpConn.Close()
				c.udpConn = nil
			}
			c.udpMu.Unlock()
			c.setUDPCtrlStatus("offline")
			return
		case <-c.done:
			return
		}
	}
}

func (c *client) writeUDP(msgType byte, id string, payload []byte) error {
	c.udpMu.Lock()
	conn := c.udpConn
	key := c.key
	ready := c.udpReady
	c.udpMu.Unlock()
	if conn == nil {
		return errors.New("udp chưa sẵn sàng")
	}
	if !ready && msgType != udpMsgHandshake && msgType != udpMsgPing {
		if debugUDP {
			log.Printf("[client] cảnh báo: gửi UDP khi handshake chưa hoàn tất (msg=%d)", msgType)
		}
	}
	buf := buildUDPMessage(msgType, key, id, payload)
	_, err := conn.Write(buf)
	if debugUDP && err == nil && msgType != udpMsgHandshake && !ready {
		log.Printf("[client] cảnh báo: gửi UDP nhưng handshake chưa được xác nhận")
	}
	return err
}

func (c *client) startUDPPing() {
	c.udpCtrlMu.Lock()
	if c.udpPingTicker != nil {
		c.udpCtrlMu.Unlock()
		return
	}
	ticker := time.NewTicker(udpControlInterval)
	stopCh := make(chan struct{})
	c.udpPingTicker = ticker
	c.udpPingStop = stopCh
	c.udpLastPong = time.Now()
	c.udpControlWarned = false
	c.udpCtrlMu.Unlock()
	c.setUDPCtrlStatus("pinging")
	go c.udpPingLoop(ticker, stopCh)
}

func (c *client) stopUDPPing() {
	c.udpCtrlMu.Lock()
	if c.udpPingTicker != nil {
		c.udpPingTicker.Stop()
		c.udpPingTicker = nil
	}
	if c.udpPingStop != nil {
		close(c.udpPingStop)
		c.udpPingStop = nil
	}
	c.udpControlWarned = false
	c.udpCtrlMu.Unlock()
}

func (c *client) udpPingLoop(ticker *time.Ticker, stopCh chan struct{}) {
	for {
		select {
		case <-ticker.C:
			ts := time.Now()
			payload := make([]byte, 8)
			binary.BigEndian.PutUint64(payload, uint64(ts.UnixNano()))
			c.udpCtrlMu.Lock()
			c.udpLastPing = ts
			c.udpCtrlMu.Unlock()
			if err := c.sendUDPPing(payload); err != nil && debugUDP {
				log.Printf("[client] gửi udp_ping lỗi: %v", err)
			}
			c.checkUDPPingTimeout()
		case <-stopCh:
			return
		case <-c.done:
			return
		}
	}
}

func (c *client) checkUDPPingTimeout() {
	c.udpCtrlMu.Lock()
	last := c.udpLastPong
	warned := c.udpControlWarned
	if time.Since(last) > udpControlTimeout {
		if !warned {
			c.udpControlWarned = true
			c.udpCtrlMu.Unlock()
			c.setUDPCtrlStatus("timeout")
			log.Printf("[client] UDP control timeout (>%v)", udpControlTimeout)
			return
		}
		c.udpCtrlMu.Unlock()
		return
	}
	if warned {
		c.udpControlWarned = false
	}
	c.udpCtrlMu.Unlock()
}

func (c *client) handleUDPPong(payload []byte) {
	if len(payload) < 8 {
		if debugUDP {
			log.Printf("[client] udp_pong payload quá ngắn")
		}
		return
	}
	sent := int64(binary.BigEndian.Uint64(payload))
	now := time.Now()
	rtt := time.Duration(now.UnixNano()-sent) * time.Nanosecond
	c.udpCtrlMu.Lock()
	c.udpLastPong = now
	c.udpControlWarned = false
	c.udpCtrlMu.Unlock()
	c.setUDPCtrlStatus(fmt.Sprintf("ok (%d ms)", rtt.Milliseconds()))
	if debugUDP {
		log.Printf("[client] nhận udp_pong, rtt %d ms", rtt.Milliseconds())
	}
}

func (c *client) setUDPCtrlStatus(status string) {
	c.udpCtrlMu.Lock()
	c.udpCtrlStatus = status
	c.udpCtrlMu.Unlock()
}

func (c *client) getUDPCtrlStatus() string {
	if strings.ToLower(c.protocol) != "udp" {
		return "n/a"
	}
	c.udpCtrlMu.Lock()
	status := c.udpCtrlStatus
	c.udpCtrlMu.Unlock()
	if status == "" {
		return "unknown"
	}
	return status
}

func (c *client) getSessionStats() (active int, total uint64) {
	c.udpMu.Lock()
	active = len(c.udpSessions)
	c.udpMu.Unlock()
	total = atomic.LoadUint64(&c.totalUDPSessions)
	return
}

func decodeUDPField(packet []byte, offset int) (string, int, bool) {
	if offset+2 > len(packet) {
		return "", offset, false
	}
	l := int(binary.BigEndian.Uint16(packet[offset : offset+2]))
	offset += 2
	if l < 0 || offset+l > len(packet) {
		return "", offset, false
	}
	return string(packet[offset : offset+l]), offset + l, true
}

func buildUDPMessage(msgType byte, key, id string, payload []byte) []byte {
	keyLen := len(key)
	idLen := len(id)
	total := 1 + 2 + keyLen
	if msgType != udpMsgHandshake {
		total += 2 + idLen
	}
	total += len(payload)
	buf := make([]byte, total)
	buf[0] = msgType
	binary.BigEndian.PutUint16(buf[1:], uint16(keyLen))
	copy(buf[3:], key)
	offset := 3 + keyLen
	if msgType != udpMsgHandshake {
		binary.BigEndian.PutUint16(buf[offset:], uint16(idLen))
		offset += 2
		copy(buf[offset:], id)
		offset += idLen
	}
	copy(buf[offset:], payload)
	return buf
}

func (c *client) reportProxyError(id string, err error) {
	if c.enc == nil {
		return
	}
	_ = c.enc.Encode(common.Message{
		Type:  "proxy_error",
		ID:    id,
		Error: err.Error(),
	})
}

func (c *client) closeControl() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
	c.closeAllUDPSessions()
	c.stopUDPPing()
	c.setUDPCtrlStatus("offline")
	c.udpMu.Lock()
	if c.udpConn != nil {
		c.udpConn.Close()
		c.udpConn = nil
	}
	c.udpReady = false
	c.udpMu.Unlock()
	if c.control != nil {
		c.control.Close()
	}
	c.control = nil
	c.enc = nil
	c.dec = nil
	if c.trafficQuit != nil {
		close(c.trafficQuit)
		c.trafficQuit = nil
	}
	if c.statusCh != nil {
		close(c.statusCh)
		c.statusCh = nil
	}
	if c.pingCh != nil {
		close(c.pingCh)
		c.pingCh = nil
	}
}

func (c *client) closeUI() {
	if c.tui != nil {
		c.tui.Close()
		c.tui = nil
	}
}

func normalizedArgs(input []string) []string {
	filtered := make([]string, 0, len(input))
	for _, arg := range input {
		if arg == "" {
			continue
		}
		if arg == os.Args[0] || strings.HasSuffix(arg, "/"+filepath.Base(os.Args[0])) {
			continue
		}
		if strings.Contains(arg, "/") {
			// likely a path accidentally forwarded via shell wrapper
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func formatRate(delta uint64, interval time.Duration) string {
	if interval <= 0 {
		return formatBytes(delta)
	}
	perSecond := float64(delta) / interval.Seconds()
	return formatBytesFloat(perSecond)
}

func formatSince(t time.Time) string {
	if t.IsZero() {
		return "N/A"
	}
	d := time.Since(t)
	if d < time.Millisecond {
		return "just now"
	}
	if d < time.Second {
		return fmt.Sprintf("%d ms ago", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1f s ago", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1f m ago", d.Minutes())
	}
	return fmt.Sprintf("%.1f h ago", d.Hours())
}

func formatBytes(n uint64) string {
	return formatBytesFloat(float64(n))
}

func formatBytesFloat(value float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	if value < 0 {
		value = 0
	}
	unit := 0
	for unit < len(units)-1 && value >= 1024 {
		value /= 1024
		unit++
	}
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f %s", value, units[unit])
	case value >= 10:
		return fmt.Sprintf("%.1f %s", value, units[unit])
	default:
		return fmt.Sprintf("%.2f %s", value, units[unit])
	}
}

type byteCounter struct {
	counter *uint64
}

func (b *byteCounter) Write(p []byte) (int, error) {
	if len(p) > 0 && b.counter != nil {
		atomic.AddUint64(b.counter, uint64(len(p)))
	}
	return len(p), nil
}

func proxyCopyCount(dst, src net.Conn, counter *uint64) {
	defer dst.Close()
	defer src.Close()
	reader := io.TeeReader(src, &byteCounter{counter: counter})
	_, _ = io.Copy(dst, reader)
}

func (c *client) recordPingReply() {
	sent := atomic.SwapInt64(&c.pingSent, 0)
	if sent <= 0 {
		return
	}
	ms := time.Since(time.Unix(0, sent))
	atomic.StoreInt64(&c.pingMs, ms.Milliseconds())
	if c.pingCh == nil {
		return
	}
	select {
	case c.pingCh <- ms:
	default:
		select {
		case <-c.pingCh:
		default:
		}
		select {
		case c.pingCh <- ms:
		default:
		}
	}
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatPingDisplay(d time.Duration) (string, string) {
	if d < 0 {
		return "N/A", "[----]"
	}
	ms := d.Milliseconds()
	var bars string
	switch {
	case ms <= 50:
		bars = "[||||]"
	case ms <= 120:
		bars = "[||| ]"
	case ms <= 250:
		bars = "[||  ]"
	case ms <= 500:
		bars = "[|   ]"
	default:
		bars = "[    ]"
	}
	return fmt.Sprintf("%d ms", ms), bars
}

func (c *client) renderFrame(stats trafficStats, ping time.Duration) {
	activeSessions, totalSessions := c.getSessionStats()
	lines := []string{
		"Quangdev - Kami2k1 | Tunnel Viet Nam Free",
		fmt.Sprintf("Local : %s", c.localAddr),
		fmt.Sprintf("Public: %s", nonEmpty(c.publicHost, "pending...")),
		fmt.Sprintf("Protocol: %s", strings.ToUpper(nonEmpty(c.protocol, "tcp"))),
		fmt.Sprintf("Traffic: ▲ %s/s ▼ %s/s | ↑ %s ↓ %s", stats.upRate, stats.downRate, stats.totalUp, stats.totalDown),
		fmt.Sprintf("UDP Control: %s", c.getUDPCtrlStatus()),
		fmt.Sprintf("Last Server Data: %s", formatSince(c.getLastServerData())),
		fmt.Sprintf("Last Backend Data: %s", formatSince(c.getLastBackendData())),
		fmt.Sprintf("Sessions: active %d | total %d", activeSessions, totalSessions),
		fmt.Sprintf("Key: %s", nonEmpty(c.key, "(none)")),
		fmt.Sprintf("Ngôn Ngữ : golang"),
		fmt.Sprintf("Phiên Bản : %s", common.Version),
	}
	pingText, bars := formatPingDisplay(ping)
	lines = append(lines, fmt.Sprintf("Ping: %s %s", pingText, bars))

	if c.tui != nil {
		c.tui.Render(lines)
		return
	}

	width, height := terminalSize()
	if width < 20 {
		width = 20
	}
	if height < 6 {
		height = 6
	}

	innerWidth := 0
	for _, line := range lines {
		if w := len([]rune(line)); w > innerWidth {
			innerWidth = w
		}
	}
	maxInner := width - 4
	if maxInner < 10 {
		maxInner = innerWidth
	}
	if maxInner > innerWidth {
		innerWidth = maxInner
	}
	frameWidth := innerWidth + 4
	indentWidth := 0
	if width > frameWidth {
		indentWidth = (width - frameWidth) / 2
	}
	indent := strings.Repeat(" ", indentWidth)
	blankLine := indent + strings.Repeat(" ", frameWidth) + "\n"

	fmt.Print("\033[H")
	var builder strings.Builder

	topPad := 0
	if height > len(lines)+2 {
		topPad = (height - (len(lines) + 2)) / 2
	}
	for i := 0; i < topPad; i++ {
		builder.WriteString(blankLine)
	}

	border := "+" + strings.Repeat("=", innerWidth+2) + "+"
	builder.WriteString(indent)
	builder.WriteString(border)
	builder.WriteByte('\n')
	for _, line := range lines {
		pad := innerWidth - len([]rune(line))
		builder.WriteString(indent)
		builder.WriteString("| ")
		builder.WriteString(line)
		builder.WriteString(strings.Repeat(" ", pad))
		builder.WriteString(" |\n")
	}
	builder.WriteString(indent)
	builder.WriteString(border)
	builder.WriteByte('\n')

	consumed := topPad + len(lines) + 2
	for consumed < height {
		builder.WriteString(blankLine)
		consumed++
	}

	fmt.Print(builder.String())
}

func terminalSize() (int, int) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			return width, height
		}
	}
	return 80, 24
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}
