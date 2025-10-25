package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kami/ngrok/common"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

const (
	defaultServerAddr = "103.78.0.204:8881"
	defaultLocalHost  = "localhost"
	defaultLocalPort  = 80
	heartbeatInterval = 20 * time.Second
)

type client struct {
	serverAddr string
	localAddr  string
	key        string
	clientID   string
	remotePort int
	publicHost string

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

	args := flag.Args()
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

	cl := &client{
		serverAddr: *serverAddr,
		localAddr:  net.JoinHostPort(localHost, strconv.Itoa(localPort)),
		clientID:   id,
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
		}
	}()

	register := common.Message{
		Type:     "register",
		Key:      c.key,
		ClientID: c.clientID,
		Target:   c.localAddr,
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
	hostPart := c.serverAddr
	if host, _, err := net.SplitHostPort(c.serverAddr); err == nil {
		hostPart = host
	}
	c.publicHost = net.JoinHostPort(hostPart, strconv.Itoa(c.remotePort))
	log.Printf("[client] đăng ký thành công, public port %d", c.remotePort)
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

func formatRate(delta uint64, interval time.Duration) string {
	if interval <= 0 {
		return formatBytes(delta)
	}
	perSecond := float64(delta) / interval.Seconds()
	return formatBytesFloat(perSecond)
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
	lines := []string{
		"Quangdev - Kami2k1 | Tunnel Viet Nam Free",
		fmt.Sprintf("Local : %s", c.localAddr),
		fmt.Sprintf("Public: %s", nonEmpty(c.publicHost, "pending...")),
		fmt.Sprintf("Traffic: ▲ %s/s ▼ %s/s | ↑ %s ↓ %s", stats.upRate, stats.downRate, stats.totalUp, stats.totalDown),
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
