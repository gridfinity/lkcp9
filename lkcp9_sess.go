// Copyright © 2015 Daniel Fu <daniel820313@gmail.com>.
// Copyright © 2019 Loki 'l0k18' Verloren <stalker.loki@protonmail.ch>.
// Copyright © 2020 Gridfinity, LLC. <admin@gridfinity.com>.
// Copyright © 2020 Jeffrey H. Johnson <jeff@gridfinity.com>.
//
// All rights reserved.
//
// All use of this code is governed by the MIT license.
// The complete license is available in the LICENSE file.

package lkcp9 // import "go.gridfinity.dev/lkcp9"

import (
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type errTimeout struct {
	error
}

func (
	errTimeout,
) Timeout() bool {
	return true
}

func (
	errTimeout,
) Temporary() bool {
	return true
}

func (
	errTimeout,
) Error() string {
	return "i/o timeout"
}

const (
	nonceSize       = 16
	crcSize         = 4
	cryptHeaderSize = nonceSize + crcSize
	KcpMtuLimit     = 1500
	rxFECMulti      = 3
	acceptBacklog   = 128
)

const (
	errBrokenPipe       = "broken pipe"
	errInvalidOperation = "invalid operation"
)

var KxmitBuf sync.Pool

func init() {
	KxmitBuf.New = func() interface{} {
		return make(
			[]byte,
			KcpMtuLimit,
		)
	}
}

type (
	UDPSession struct {
		updaterIdx   int            // record slice index in updater
		conn         net.PacketConn // the underlying packet connection
		Kcp          *KCP           // KCP ARQ protocol
		l            *Listener      // pointing to the Listener object if it's been accepted by a Listener
		block        BlockCrypt     // block encryption object
		recvbuf      []byte
		bufptr       []byte
		FecDecoder   *FecDecoder
		FecEncoder   *FecEncoder
		remote       net.Addr      // remote peer address
		rd           time.Time     // read deadline
		wd           time.Time     // write deadline
		headerSize   int           // the header size additional to a KCP frame
		ackNoDelay   bool          // send ack immediately for each incoming packet(testing purpose)
		writeDelay   bool          // delay Kcp.flush() for Write() for bulk transfer
		dup          int           // duplicate udp packets(testing purpose)
		die          chan struct{} // notify current session has Closed
		chReadEvent  chan struct{} // notify Read() can be called without blocking
		chWriteEvent chan struct{} // notify Write() can be called without blocking
		chReadError  chan error    // notify PacketConn.Read() have an error
		chWriteError chan error    // notify PacketConn.Write() have an error
		nonce        Entropy
		isClosed     bool // flag the session has Closed
		mu           sync.Mutex
	}

	setReadBuffer interface {
		SetReadBuffer(
			bytes int,
		) error
	}

	setWriteBuffer interface {
		SetWriteBuffer(
			bytes int,
		) error
	}
)

// newUDPSession creates a new UDP session (client or server)
func newUDPSession(
	conv uint32,
	dataShards,
	parityShards int,
	l *Listener,
	conn net.PacketConn,
	remote net.Addr,
	block BlockCrypt,
) *UDPSession {
	sess := new(
		UDPSession,
	)
	sess.die = make(
		chan struct{},
	)
	sess.nonce = new(
		KcpNonceAES128,
	)
	sess.nonce.Init()
	sess.chReadEvent = make(
		chan struct{},
		1,
	)
	sess.chWriteEvent = make(
		chan struct{},
		1,
	)
	sess.chReadError = make(
		chan error,
		1,
	)
	sess.chWriteError = make(
		chan error,
		1,
	)
	sess.remote = remote
	sess.conn = conn
	sess.l = l
	sess.block = block
	sess.recvbuf = make(
		[]byte,
		KcpMtuLimit,
	)
	sess.FecDecoder = KcpNewDECDecoder(
		rxFECMulti*(dataShards+parityShards),
		dataShards,
		parityShards,
	)
	if sess.block != nil {
		sess.FecEncoder = KcpNewDECEncoder(
			dataShards,
			parityShards,
			cryptHeaderSize,
		)
	} else {
		sess.FecEncoder = KcpNewDECEncoder(
			dataShards,
			parityShards,
			0,
		)
	}
	if sess.block != nil {
		sess.headerSize += cryptHeaderSize
	}
	if sess.FecEncoder != nil {
		sess.headerSize += fecHeaderSizePlus2
	}
	sess.Kcp = NewKCP(conv, func(
		buf []byte,
		size int,
	) {
		if size >= IKCP_OVERHEAD+sess.headerSize {
			sess.output(
				buf[:size],
			)
		}
	})
	sess.Kcp.ReserveBytes(
		sess.headerSize,
	)
	updater.addSession(
		sess,
	)
	if sess.l == nil {
		go sess.readLoop()
		atomic.AddUint64(
			&DefaultSnsi.KcpActiveOpen,
			1,
		)
	} else {
		atomic.AddUint64(
			&DefaultSnsi.KcpPassiveOpen,
			1,
		)
	}
	currestab := atomic.AddUint64(
		&DefaultSnsi.KcpNowEstablished,
		1,
	)
	maxconn := atomic.LoadUint64(
		&DefaultSnsi.MaxConn,
	)
	if currestab > maxconn {
		atomic.CompareAndSwapUint64(
			&DefaultSnsi.MaxConn,
			maxconn,
			currestab,
		)
	}
	return sess
}

// Read implements net.Conn
// Function is safe for concurrent access.
func (
	s *UDPSession,
) Read(
	b []byte,
) (
	n int,
	err error,
) {
	for {
		s.mu.Lock()
		if len(
			s.bufptr,
		) > 0 {
			n = copy(
				b,
				s.bufptr,
			)
			s.bufptr = s.bufptr[n:]
			s.mu.Unlock()
			atomic.AddUint64(
				&DefaultSnsi.KcpBytesReceived,
				uint64(n),
			)
			return n, nil
		}
		if s.isClosed {
			s.mu.Unlock()
			return 0, errors.New(
				errBrokenPipe,
			)
		}
		if size := s.Kcp.PeekSize(); size > 0 {
			if len(b) >= size {
				s.Kcp.Recv(
					b,
				)
				s.mu.Unlock()
				atomic.AddUint64(&DefaultSnsi.KcpBytesReceived, uint64(size))
				return size, nil
			}
			if cap(s.recvbuf) < size {
				s.recvbuf = make([]byte, size)
			}
			s.recvbuf = s.recvbuf[:size]
			s.Kcp.Recv(
				s.recvbuf,
			)
			n = copy(
				b,
				s.recvbuf,
			)
			s.bufptr = s.recvbuf[n:]
			s.mu.Unlock()
			atomic.AddUint64(
				&DefaultSnsi.KcpBytesReceived,
				uint64(n),
			)
			return n, nil
		}
		var timeout *time.Timer
		var c <-chan time.Time
		if !s.rd.IsZero() {
			if time.Now().After(
				s.rd,
			) {
				s.mu.Unlock()
				return 0, errTimeout{}
			}
			delay := s.rd.Sub(
				time.Now(),
			)
			timeout = time.NewTimer(
				delay,
			)
			c = timeout.C
		}
		s.mu.Unlock()
		select {
		case <-s.chReadEvent:
		case <-c:
		case <-s.die:
		case err = <-s.chReadError:
			if timeout != nil {
				timeout.Stop()
			}
			return n, err
		}

		if timeout != nil {
			timeout.Stop()
		}
	}
}

func (
	s *UDPSession,
) Write(
	b []byte,
) (
	n int,
	err error,
) {
	return s.WriteBuffers(
		[][]byte{b},
	)
}

func (
	s *UDPSession,
) WriteBuffers(
	v [][]byte,
) (
	n int,
	err error,
) {
	for {
		s.mu.Lock()
		if s.isClosed {
			s.mu.Unlock()
			return 0,
				errors.New(
					errBrokenPipe,
				)
		}

		if s.Kcp.WaitSnd() < int(
			s.Kcp.snd_wnd,
		) {
			for _, b := range v {
				n += len(
					b)
				for {
					if len(
						b,
					) <= int(
						s.Kcp.mss,
					) {
						s.Kcp.Send(
							b,
						)
						break
					} else {
						s.Kcp.Send(
							b[:s.Kcp.mss],
						)
						b = b[s.Kcp.mss:]
					}
				}
			}

			if s.Kcp.WaitSnd() >= int(s.Kcp.snd_wnd) || !s.writeDelay {
				s.Kcp.Flush(
					false,
				)
			}
			s.mu.Unlock()
			atomic.AddUint64(
				&DefaultSnsi.KcpBytesSent,
				uint64(n),
			)
			return n, nil
		}

		var timeout *time.Timer
		var c <-chan time.Time
		if !s.wd.IsZero() {
			if time.Now().After(s.wd) {
				s.mu.Unlock()
				return 0, errTimeout{}
			}
			delay := s.wd.Sub(time.Now())
			timeout = time.NewTimer(delay)
			c = timeout.C
		}
		s.mu.Unlock()

		select {
		case <-s.chWriteEvent:
		case <-c:
		case <-s.die:
		case err = <-s.chWriteError:
			if timeout != nil {
				timeout.Stop()
			}
			return n, err
		}

		if timeout != nil {
			timeout.Stop()
		}
	}
}

func (
	s *UDPSession,
) Close() error {
	updater.removeSession(s)
	if s.l != nil {
		s.l.CloseSession(
			s.remote,
		)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return errors.New(
			errBrokenPipe,
		)
	}
	close(
		s.die,
	)
	s.isClosed = true
	atomic.AddUint64(
		&DefaultSnsi.KcpNowEstablished,
		^uint64(0),
	)
	if s.l == nil {
		return s.conn.Close()
	}
	return nil
}

// LocalAddr returns the local network address.
// The address returned is shared by all invocations of LocalAddr - do not modify it.
func (
	s *UDPSession,
) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
// The adress returned is shared by all invocations of RemoteAddr - do not modify it.
func (
	s *UDPSession,
) RemoteAddr() net.Addr {
	return s.remote
}

// SetDeadline sets a deadline associated with the listener.
// A zero time value disables a deadline.
func (
	s *UDPSession,
) SetDeadline(
	t time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	s.wd = t
	s.notifyReadEvent()
	s.notifyWriteEvent()
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (
	s *UDPSession,
) SetReadDeadline(
	t time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	s.notifyReadEvent()
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (
	s *UDPSession,
) SetWriteDeadline(
	t time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wd = t
	s.notifyWriteEvent()
	return nil
}

// SetWriteDelay delays writes for bulk transfers, until the next update interval.
func (
	s *UDPSession,
) SetWriteDelay(
	delay bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeDelay = delay
}

// SetWindowSize sets the maximum window size
func (
	s *UDPSession,
) SetWindowSize(
	sndwnd,
	rcvwnd int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Kcp.WndSize(
		sndwnd,
		rcvwnd,
	)
}

// SetMtu sets the maximum transmission unit
// This size does not including UDP header itself.
func (
	s *UDPSession,
) SetMtu(
	mtu int,
) bool {
	if mtu > KcpMtuLimit {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Kcp.SetMtu(
		mtu,
	)
	return true
}

// SetStreamMode toggles the streaming mode on or off
func (s *UDPSession) SetStreamMode(
	enable bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enable {
		s.Kcp.stream = 1
	} else {
		s.Kcp.stream = 0
	}
}

// SetACKNoDelay changes the ACK flushing option.
// If set to true, ACKs are flusghed immediately,
func (
	s *UDPSession,
) SetACKNoDelay(
	nodelay bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackNoDelay = nodelay
}

// SetDUP duplicates UDP packets for Kcp output.
// Useful for testing, not for normal use.
func (
	s *UDPSession,
) SetDUP(
	dup int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dup = dup
}

// SetNoDelay sets TCP_DELAY, for Kcp.
func (
	s *UDPSession,
) SetNoDelay(
	nodelay,
	interval,
	resend,
	nc int,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Kcp.NoDelay(
		nodelay,
		interval,
		resend,
		nc,
	)
}

// SetDSCP sets the 6-bit DSCP field of IP header.
// Has no effect, unless accepted by your Listener.
func (
	s *UDPSession,
) SetDSCP(
	dscp int,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(net.Conn); ok {
			addr, _ := net.ResolveUDPAddr(
				"udp",
				nc.LocalAddr().String(),
			)
			if addr.IP.To4() != nil {
				return ipv4.NewConn(nc).SetTOS(
					dscp << 2,
				)
			} else {
				return ipv6.NewConn(nc).SetTrafficClass(
					dscp,
				)
			}
		}
	}
	return errors.New(
		errInvalidOperation,
	)
}

// SetReadBuffer sets the socket read buffer.
// Has no effect, unless it's accepted by your Listener.
func (
	s *UDPSession,
) SetReadBuffer(
	bytes int,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(setReadBuffer); ok {
			return nc.SetReadBuffer(
				bytes,
			)
		}
	}
	return errors.New(
		errInvalidOperation,
	)
}

// SetWriteBuffer sets the socket write buffer.
// Has no effect, unless it's accepted by your Listener.
func (
	s *UDPSession,
) SetWriteBuffer(
	bytes int,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		if nc, ok := s.conn.(setWriteBuffer); ok {
			return nc.SetWriteBuffer(
				bytes,
			)
		}
	}
	return errors.New(
		errInvalidOperation,
	)
}

func (
	s *UDPSession,
) output(
	buf []byte,
) {
	var ecc [][]byte
	if s.FecEncoder != nil {
		ecc = s.FecEncoder.Encode(
			buf,
		)
	}
	if s.block != nil {
		s.nonce.Fill(
			buf[:nonceSize],
		)
		checksum := crc32.ChecksumIEEE(
			buf[cryptHeaderSize:],
		)
		binary.LittleEndian.PutUint32(
			buf[nonceSize:],
			checksum,
		)
		s.block.Encrypt(
			buf,
			buf,
		)
		for k := range ecc {
			s.nonce.Fill(ecc[k][:nonceSize])
			checksum := crc32.ChecksumIEEE(
				ecc[k][cryptHeaderSize:],
			)
			binary.LittleEndian.PutUint32(
				ecc[k][nonceSize:],
				checksum,
			)
			s.block.Encrypt(ecc[k],
				ecc[k],
			)
		}
	}
	nbytes := 0
	npkts := 0
	for i := 0; i < s.dup+1; i++ {
		if n, err := s.conn.WriteTo(
			buf,
			s.remote,
		); err == nil {
			nbytes += n
			npkts++
		} else {
			s.notifyWriteError(
				err,
			)
		}
	}
	for k := range ecc {
		if n, err := s.conn.WriteTo(
			ecc[k],
			s.remote,
		); err == nil {
			nbytes += n
			npkts++
		} else {
			s.notifyWriteError(
				err,
			)
		}
	}
	atomic.AddUint64(
		&DefaultSnsi.KcpOutputPackets,
		uint64(npkts),
	)
	atomic.AddUint64(
		&DefaultSnsi.KcpOutputBytes,
		uint64(nbytes),
	)
}

func (
	s *UDPSession,
) update() (
	interval time.Duration,
) {
	s.mu.Lock()
	waitsnd := s.Kcp.WaitSnd()
	interval = time.Duration(
		s.Kcp.Flush(false)) * time.Millisecond
	if s.Kcp.WaitSnd() < waitsnd {
		s.notifyWriteEvent()
	}
	s.mu.Unlock()
	return
}

func (
	s *UDPSession,
) GetConv() uint32 {
	return s.Kcp.conv
}

func (
	s *UDPSession,
) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

func (
	s *UDPSession,
) notifyWriteEvent() {
	select {
	case s.chWriteEvent <- struct{}{}:
	default:
	}
}

func (
	s *UDPSession,
) notifyWriteError(err error) {
	select {
	case s.chWriteError <- err:
	default:
	}
}

func (
	s *UDPSession,
) packetInput(
	data []byte,
) {
	dataValid := false
	if s.block != nil {
		s.block.Decrypt(
			data,
			data,
		)
		data = data[nonceSize:]
		checksum := crc32.ChecksumIEEE(
			data[crcSize:],
		)
		if checksum == binary.LittleEndian.Uint32(
			data,
		) {
			data = data[crcSize:]
			dataValid = true
		} else {
			atomic.AddUint64(
				&DefaultSnsi.KcpChecksumFailures,
				1,
			)
		}
	} else if s.block == nil {
		dataValid = true
	}

	if dataValid {
		s.KcpInput(
			data,
		)
	}
}

func (
	s *UDPSession,
) KcpInput(
	data []byte,
) {
	var KcpInErrors,
		fecErrs,
		fecRecovered,
		fecParityShards uint64
	if s.FecDecoder != nil {
		if len(
			data,
		) > fecHeaderSize {
			f := FecPacket(
				data,
			)
			if f.flag() == KTypeData || f.flag() == KTypeParity {
				if f.flag() == KTypeParity {
					fecParityShards++
				}
				recovers := s.FecDecoder.Decode(
					f,
				)
				s.mu.Lock()
				waitsnd := s.Kcp.WaitSnd()
				if f.flag() == KTypeData {
					if ret := s.Kcp.Input(
						data[fecHeaderSizePlus2:],
						true,
						s.ackNoDelay,
					); ret != 0 {
						KcpInErrors++
					}
				}
				for _, r := range recovers {
					if len(
						r,
					) >= 2 {
						sz := binary.LittleEndian.Uint16(
							r,
						)
						if int(
							sz,
						) <= len(
							r,
						) && sz >= 2 {
							if ret := s.Kcp.Input(
								r[2:sz],
								false,
								s.ackNoDelay,
							); ret == 0 {
								fecRecovered++
							} else {
								KcpInErrors++
							}
						} else {
							fecErrs++
						}
					} else {
						fecErrs++
					}
					KxmitBuf.Put(
						r,
					)
				}
				if n := s.Kcp.PeekSize(); n > 0 {
					s.notifyReadEvent()
				}
				if s.Kcp.WaitSnd() < waitsnd {
					s.notifyWriteEvent()
				}
				s.mu.Unlock()
			} else {
				atomic.AddUint64(
					&DefaultSnsi.KcpPreInputErrors,
					1,
				)
			}
		} else {
			atomic.AddUint64(
				&DefaultSnsi.KcpInputErrors,
				1,
			)
		}
	} else {
		s.mu.Lock()
		waitsnd := s.Kcp.WaitSnd()
		if ret := s.Kcp.Input(
			data,
			true,
			s.ackNoDelay,
		); ret != 0 {
			KcpInErrors++
		}
		if n := s.Kcp.PeekSize(); n > 0 {
			s.notifyReadEvent()
		}
		if s.Kcp.WaitSnd() < waitsnd {
			s.notifyWriteEvent()
		}
		s.mu.Unlock()
	}
	atomic.AddUint64(
		&DefaultSnsi.KcpInputPackets,
		1,
	)
	atomic.AddUint64(
		&DefaultSnsi.KcpInputBytes,
		uint64(len(
			data,
		)))
	if fecParityShards > 0 {
		atomic.AddUint64(
			&DefaultSnsi.KcpFECParityShards,
			fecParityShards,
		)
	}
	if KcpInErrors > 0 {
		atomic.AddUint64(
			&DefaultSnsi.KcpInputErrors,
			KcpInErrors,
		)
	}
	if fecErrs > 0 {
		atomic.AddUint64(
			&DefaultSnsi.KcpFailures,
			fecErrs,
		)
	}
	if fecRecovered > 0 {
		atomic.AddUint64(
			&DefaultSnsi.KcpFECRecovered,
			fecRecovered,
		)
	}
}

type (
	Listener struct {
		block           BlockCrypt             // block encryption
		dataShards      int                    // FEC data shard
		parityShards    int                    // FEC parity shard
		FecDecoder      *FecDecoder            // FEC mock initialization
		conn            net.PacketConn         // the underlying packet connection
		sessions        map[string]*UDPSession // all sessions accepted by this Listener
		sessionLock     sync.Mutex
		chAccepts       chan *UDPSession // Listen() backlog
		chSessionClosed chan net.Addr    // session close queue
		headerSize      int              // additional header for a Kcp frame
		die             chan struct{}    // notify when the Listener has closed
		rd              atomic.Value     // read deadline for Accept()
		wd              atomic.Value
	}
)

func (
	l *Listener,
) packetInput(
	data []byte,
	addr net.Addr,
) {
	dataValid := false
	if l.block != nil {
		l.block.Decrypt(
			data,
			data,
		)
		data = data[nonceSize:]
		checksum := crc32.ChecksumIEEE(
			data[crcSize:],
		)
		if checksum == binary.LittleEndian.Uint32(
			data,
		) {
			data = data[crcSize:]
			dataValid = true
		} else {
			atomic.AddUint64(
				&DefaultSnsi.KcpChecksumFailures,
				1,
			)
		}
	} else if l.block == nil {
		dataValid = true
	}
	if dataValid {
		l.sessionLock.Lock()
		s, ok := l.sessions[addr.String()]
		l.sessionLock.Unlock()
		if !ok {
			if len(
				l.chAccepts,
			) < cap(
				l.chAccepts,
			) {
				var conv uint32
				convValid := false
				if l.FecDecoder != nil {
					isfec := binary.LittleEndian.Uint16(
						data[4:],
					)
					if isfec == KTypeData {
						conv = binary.LittleEndian.Uint32(
							data[fecHeaderSizePlus2:],
						)
						convValid = true
					}
				} else {
					conv = binary.LittleEndian.Uint32(
						data,
					)
					convValid = true
				}

				if convValid {
					s := newUDPSession(
						conv,
						l.dataShards,
						l.parityShards,
						l,
						l.conn,
						addr,
						l.block,
					)
					s.KcpInput(
						data,
					)
					l.sessionLock.Lock()
					l.sessions[addr.String()] = s
					l.sessionLock.Unlock()
					l.chAccepts <- s
				}
			}
		} else {
			s.KcpInput(
				data,
			)
		}
	}
}

// SetReadBuffer sets the socket read buffer for the Listener.
func (
	l *Listener,
) SetReadBuffer(
	bytes int,
) error {
	if nc, ok := l.conn.(setReadBuffer); ok {
		return nc.SetReadBuffer(
			bytes,
		)
	}
	return errors.New(
		errInvalidOperation,
	)
}

// SetWriteBuffer sets the socket write buffer for the Listener.
func (
	l *Listener,
) SetWriteBuffer(
	bytes int,
) error {
	if nc, ok := l.conn.(setWriteBuffer); ok {
		return nc.SetWriteBuffer(
			bytes,
		)
	}
	return errors.New(
		errInvalidOperation,
	)
}

// SetDSCP sets the 6-bit DSCP field of IP header.
func (
	l *Listener,
) SetDSCP(
	dscp int,
) error {
	if nc, ok := l.conn.(net.Conn); ok {
		addr, _ := net.ResolveUDPAddr(
			"udp",
			nc.LocalAddr().String(),
		)
		if addr.IP.To4() != nil {
			return ipv4.NewConn(
				nc,
			).SetTOS(
				dscp << 2,
			)
		} else {
			return ipv6.NewConn(
				nc,
			).SetTrafficClass(
				dscp,
			)
		}
	}
	return errors.New(
		errInvalidOperation,
	)
}

// Accept implements the Accept method in the Listener interface.
// It waits until the next call, then returns a generic 'Conn'.
func (
	l *Listener,
) Accept() (
	net.Conn,
	error,
) {
	return l.AcceptKCP()
}

// AcceptKCP accepts a Kcp connection
func (
	l *Listener,
) AcceptKCP() (
	*UDPSession,
	error,
) {
	var timeout <-chan time.Time
	if tdeadline, ok := l.rd.Load().(time.Time); ok && !tdeadline.IsZero() {
		timeout = time.After(
			tdeadline.Sub(
				time.Now(),
			))
	}

	select {
	case <-timeout:
		return nil, &errTimeout{}
	case c := <-l.chAccepts:
		return c, nil
	case <-l.die:
		return nil, errors.New(
			errBrokenPipe,
		)
	}
}

// SetDeadline sets the deadline associated with the Listener.
// A zero value will disable all deadlines.
func (
	l *Listener,
) SetDeadline(
	t time.Time,
) error {
	l.SetReadDeadline(
		t,
	)
	l.SetWriteDeadline(
		t,
	)
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (
	l *Listener,
) SetReadDeadline(
	t time.Time,
) error {
	l.rd.Store(
		t,
	)
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (
	l *Listener,
) SetWriteDeadline(
	t time.Time,
) error {
	l.wd.Store(
		t,
	)
	return nil
}

// Close stops listening on the UDP address.
// Any already accepted connections will not be closed.
func (
	l *Listener,
) Close() error {
	close(
		l.die,
	)
	return l.conn.Close()
}

// CloseSession notifies the Listener when a Session is Closed.
func (
	l *Listener,
) CloseSession(
	remote net.Addr,
) (
	ret bool,
) {
	l.sessionLock.Lock()
	defer l.sessionLock.Unlock()
	if _, ok := l.sessions[remote.String()]; ok {
		delete(
			l.sessions,
			remote.String(),
		)
		return true
	}
	return false
}

// Addr returns the listener's network address.
// The address returned is shared by all invocations of Addr - do not modify it.
func (
	l *Listener,
) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// Listen listens for incoming Kcp packets addressed to our local address (laddr) via "udp"
func Listen(laddr string) (
	net.Listener, error) {
	return ListenWithOptions(
		laddr,
		nil,
		0,
		0,
	)
}

// ListenWithOptions listens for incoming Kcp packets addressed to our local address (laddr) via "udp"
// Porvides for encryption, sharding, parity, and RS coding parameters to be specified.
func ListenWithOptions(
	laddr string,
	block BlockCrypt,
	dataShards,
	parityShards int,
) (
	*Listener,
	error,
) {
	udpaddr, err := net.ResolveUDPAddr(
		"udp",
		laddr,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"net.ResolveUDPAddr",
		)
	}
	conn, err := net.ListenUDP(
		"udp",
		udpaddr,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"net.ListenUDP",
		)
	}
	return ServeConn(
		block,
		dataShards,
		parityShards,
		conn,
	)
}

// ServeConn serves the Kcp protocol - a single packet is processed.
func ServeConn(
	block BlockCrypt,
	dataShards,
	parityShards int,
	conn net.PacketConn,
) (
	*Listener,
	error,
) {
	l := new(
		Listener,
	)
	l.conn = conn
	l.sessions = make(
		map[string]*UDPSession,
	)
	l.chAccepts = make(
		chan *UDPSession,
		acceptBacklog,
	)
	l.chSessionClosed = make(
		chan net.Addr,
	)
	l.die = make(
		chan struct{},
	)
	l.dataShards = dataShards
	l.parityShards = parityShards
	l.block = block
	l.FecDecoder = KcpNewDECDecoder(
		rxFECMulti*(dataShards+parityShards), dataShards, parityShards)
	if l.block != nil {
		l.headerSize += cryptHeaderSize
	}
	if l.FecDecoder != nil {
		l.headerSize += fecHeaderSizePlus2
	}
	go l.monitor()
	return l, nil
}

// Dial connects to the remote address "raddr" via "udp"
func Dial(
	raddr string,
) (
	net.Conn,
	error,
) {
	return DialWithOptions(
		raddr,
		nil,
		0,
		0,
	)
}

// DialWithOptions connects to the remote address "raddr" via "udp" with encryption options.
func DialWithOptions(
	raddr string,
	block BlockCrypt,
	dataShards,
	parityShards int,
) (
	*UDPSession,
	error,
) {
	udpaddr, err := net.ResolveUDPAddr(
		"udp",
		raddr,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"net.ResolveUDPAddr",
		)
	}
	network := "udp4"
	if udpaddr.IP.To4() == nil {
		network = "udp"
	}
	conn, err := net.ListenUDP(
		network,
		nil,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"net.DialUDP",
		)
	}
	return NewConn(
		raddr,
		block,
		dataShards,
		parityShards,
		conn,
	)
}

// NewConn establishes a session, talking Kcp over a packet connection.
func NewConn(
	raddr string,
	block BlockCrypt,
	dataShards,
	parityShards int,
	conn net.PacketConn,
) (
	*UDPSession,
	error,
) {
	udpaddr, err := net.ResolveUDPAddr(
		"udp",
		raddr,
	)
	if err != nil {
		return nil, errors.Wrap(
			err,
			"net.ResolveUDPAddr",
		)
	}
	var convid uint32
	binary.Read(
		rand.Reader,
		binary.LittleEndian,
		&convid,
	)
	return newUDPSession(
		convid,
		dataShards,
		parityShards,
		nil,
		conn,
		udpaddr,
		block,
	), nil
}

var refTime time.Time = time.Now()

func KcpCurrentMs() uint32 {
	return uint32(time.Now().Sub(refTime) / time.Millisecond)
}
