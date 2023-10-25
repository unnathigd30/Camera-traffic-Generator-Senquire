package gortsplib

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
)

type serverSessionMedia struct {
	ss                     *ServerSession
	media                  *description.Media
	tcpChannel             int
	udpRTPReadPort         int
	udpRTPWriteAddr        *net.UDPAddr
	udpRTCPReadPort        int
	udpRTCPWriteAddr       *net.UDPAddr
	tcpRTPFrame            *base.InterleavedFrame
	tcpRTCPFrame           *base.InterleavedFrame
	tcpBuffer              []byte
	formats                map[uint8]*serverSessionFormat // record only
	writePacketRTPInQueue  func([]byte)
	writePacketRTCPInQueue func([]byte)
	onPacketRTCP           OnPacketRTCPFunc
}

func newServerSessionMedia(ss *ServerSession, medi *description.Media) *serverSessionMedia {
	sm := &serverSessionMedia{
		ss:           ss,
		media:        medi,
		onPacketRTCP: func(rtcp.Packet) {},
	}

	if ss.state == ServerSessionStatePreRecord {
		sm.formats = make(map[uint8]*serverSessionFormat)
		for _, forma := range medi.Formats {
			sm.formats[forma.PayloadType()] = newServerSessionFormat(sm, forma)
		}
	}

	return sm
}

func (sm *serverSessionMedia) start() {
	// allocate udpRTCPReceiver before udpRTCPListener
	// otherwise udpRTCPReceiver.LastSSRC() can't be called.
	for _, sf := range sm.formats {
		sf.start()
	}

	switch *sm.ss.setuppedTransport {
	case TransportUDP, TransportUDPMulticast:
		sm.writePacketRTPInQueue = sm.writePacketRTPInQueueUDP
		sm.writePacketRTCPInQueue = sm.writePacketRTCPInQueueUDP

		if *sm.ss.setuppedTransport == TransportUDP {
			if sm.ss.state == ServerSessionStatePlay {
				// firewall opening is performed with RTCP sender reports generated by ServerStream

				// readers can send RTCP packets only
				sm.ss.s.udpRTCPListener.addClient(sm.ss.author.ip(), sm.udpRTCPReadPort, sm.readRTCPUDPPlay)
			} else {
				// open the firewall by sending empty packets to the counterpart.
				sm.ss.WritePacketRTP(sm.media, &rtp.Packet{Header: rtp.Header{Version: 2}}) //nolint:errcheck
				sm.ss.WritePacketRTCP(sm.media, &rtcp.ReceiverReport{})                     //nolint:errcheck

				sm.ss.s.udpRTPListener.addClient(sm.ss.author.ip(), sm.udpRTPReadPort, sm.readRTPUDPRecord)
				sm.ss.s.udpRTCPListener.addClient(sm.ss.author.ip(), sm.udpRTCPReadPort, sm.readRTCPUDPRecord)
			}
		}

	case TransportTCP:
		sm.writePacketRTPInQueue = sm.writePacketRTPInQueueTCP
		sm.writePacketRTCPInQueue = sm.writePacketRTCPInQueueTCP

		if sm.ss.tcpCallbackByChannel == nil {
			sm.ss.tcpCallbackByChannel = make(map[int]readFunc)
		}

		if sm.ss.state == ServerSessionStatePlay {
			sm.ss.tcpCallbackByChannel[sm.tcpChannel] = sm.readRTPTCPPlay
			sm.ss.tcpCallbackByChannel[sm.tcpChannel+1] = sm.readRTCPTCPPlay
		} else {
			sm.ss.tcpCallbackByChannel[sm.tcpChannel] = sm.readRTPTCPRecord
			sm.ss.tcpCallbackByChannel[sm.tcpChannel+1] = sm.readRTCPTCPRecord
		}

		sm.tcpRTPFrame = &base.InterleavedFrame{Channel: sm.tcpChannel}
		sm.tcpRTCPFrame = &base.InterleavedFrame{Channel: sm.tcpChannel + 1}
		sm.tcpBuffer = make([]byte, sm.ss.s.MaxPacketSize+4)
	}
}

func (sm *serverSessionMedia) stop() {
	if *sm.ss.setuppedTransport == TransportUDP {
		sm.ss.s.udpRTPListener.removeClient(sm.ss.author.ip(), sm.udpRTPReadPort)
		sm.ss.s.udpRTCPListener.removeClient(sm.ss.author.ip(), sm.udpRTCPReadPort)
	}

	for _, sf := range sm.formats {
		sf.stop()
	}
}

func (sm *serverSessionMedia) findFormatWithSSRC(ssrc uint32) *serverSessionFormat {
	for _, format := range sm.formats {
		tssrc, ok := format.rtcpReceiver.SenderSSRC()
		if ok && tssrc == ssrc {
			return format
		}
	}
	return nil
}

func (sm *serverSessionMedia) writePacketRTPInQueueUDP(payload []byte) {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.ss.s.udpRTPListener.write(payload, sm.udpRTPWriteAddr) //nolint:errcheck
}

func (sm *serverSessionMedia) writePacketRTCPInQueueUDP(payload []byte) {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.ss.s.udpRTCPListener.write(payload, sm.udpRTCPWriteAddr) //nolint:errcheck
}

func (sm *serverSessionMedia) writePacketRTPInQueueTCP(payload []byte) {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.tcpRTPFrame.Payload = payload
	sm.ss.tcpConn.nconn.SetWriteDeadline(time.Now().Add(sm.ss.s.WriteTimeout))
	sm.ss.tcpConn.conn.WriteInterleavedFrame(sm.tcpRTPFrame, sm.tcpBuffer) //nolint:errcheck
}

func (sm *serverSessionMedia) writePacketRTCPInQueueTCP(payload []byte) {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.tcpRTCPFrame.Payload = payload
	sm.ss.tcpConn.nconn.SetWriteDeadline(time.Now().Add(sm.ss.s.WriteTimeout))
	sm.ss.tcpConn.conn.WriteInterleavedFrame(sm.tcpRTCPFrame, sm.tcpBuffer) //nolint:errcheck
}

func (sm *serverSessionMedia) writePacketRTP(payload []byte) error {
	ok := sm.ss.writer.push(func() {
		sm.writePacketRTPInQueue(payload)
	})
	if !ok {
		return liberrors.ErrServerWriteQueueFull{}
	}

	return nil
}

func (sm *serverSessionMedia) writePacketRTCP(payload []byte) error {
	ok := sm.ss.writer.push(func() {
		sm.writePacketRTCPInQueue(payload)
	})
	if !ok {
		return liberrors.ErrServerWriteQueueFull{}
	}

	return nil
}

func (sm *serverSessionMedia) readRTCPUDPPlay(payload []byte) {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBigUDP{})
		return
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	for _, pkt := range packets {
		sm.onPacketRTCP(pkt)
	}
}

func (sm *serverSessionMedia) readRTPUDPRecord(payload []byte) {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketTooBigUDP{})
		return
	}

	pkt := &rtp.Packet{}
	err := pkt.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	forma, ok := sm.formats[pkt.PayloadType]
	if !ok {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketUnknownPayloadType{PayloadType: pkt.PayloadType})
		return
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	forma.readRTPUDP(pkt, now)
}

func (sm *serverSessionMedia) readRTCPUDPRecord(payload []byte) {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBigUDP{})
		return
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	for _, pkt := range packets {
		if sr, ok := pkt.(*rtcp.SenderReport); ok {
			format := sm.findFormatWithSSRC(sr.SSRC)
			if format != nil {
				format.rtcpReceiver.ProcessSenderReport(sr, now)
			}
		}

		sm.onPacketRTCP(pkt)
	}
}

func (sm *serverSessionMedia) readRTPTCPPlay(_ []byte) {
}

func (sm *serverSessionMedia) readRTCPTCPPlay(payload []byte) {
	if len(payload) > udpMaxPayloadSize {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBig{L: len(payload), Max: udpMaxPayloadSize})
		return
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	for _, pkt := range packets {
		sm.onPacketRTCP(pkt)
	}
}

func (sm *serverSessionMedia) readRTPTCPRecord(payload []byte) {
	pkt := &rtp.Packet{}
	err := pkt.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	forma, ok := sm.formats[pkt.PayloadType]
	if !ok {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketUnknownPayloadType{PayloadType: pkt.PayloadType})
		return
	}

	forma.readRTPTCP(pkt)
}

func (sm *serverSessionMedia) readRTCPTCPRecord(payload []byte) {
	if len(payload) > udpMaxPayloadSize {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBig{L: len(payload), Max: udpMaxPayloadSize})
		return
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return
	}

	now := sm.ss.s.timeNow()

	for _, pkt := range packets {
		if sr, ok := pkt.(*rtcp.SenderReport); ok {
			format := sm.findFormatWithSSRC(sr.SSRC)
			if format != nil {
				format.rtcpReceiver.ProcessSenderReport(sr, now)
			}
		}

		sm.onPacketRTCP(pkt)
	}
}