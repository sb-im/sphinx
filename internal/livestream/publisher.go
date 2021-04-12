package livestream

import (
	"errors"
	"fmt"
	"io"
	"sync"

	pb "github.com/SB-IM/pb/signal"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
)

// publisher implements Livestream interface.
type publisher struct {
	// meta contains id and track source of this publisher.
	meta *pb.Meta

	config broadcastConfigOptions
	client mqtt.Client

	createTrack  func() (webrtc.TrackLocal, error)
	streamSource func() string

	// liveStream blocks indefinitely if there no error.
	liveStream func(address string, videoTrack webrtc.TrackLocal, logger *zerolog.Logger) error

	pendingCandidates []*webrtc.ICECandidate
	candidatesMux     sync.Mutex

	logger zerolog.Logger
}

func (p *publisher) Publish() error {
	p.logger = p.logger.With().Str("id", p.meta.Id).Int32("track_source", int32(p.meta.TrackSource)).Logger()
	p.logger.Info().Msg("publishing stream")

	videoTrack, err := p.createTrack()
	if err != nil {
		return err
	}
	p.logger.Debug().Msg("created video track")

	if err := p.createPeerConnection(videoTrack); err != nil {
		return fmt.Errorf("failed to create PeerConnection: %w", err)
	}
	p.logger.Debug().Msg("created PeerConnection")

	if err := p.liveStream(p.streamSource(), videoTrack, &p.logger); err != nil {
		return fmt.Errorf("live stream failed: %w", err)
	}
	p.logger.Debug().Msg("live stream is over")

	return nil
}

func (p *publisher) Meta() *pb.Meta {
	return p.meta
}

func (p *publisher) createPeerConnection(videoTrack webrtc.TrackLocal) error {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs:       []string{p.config.ICEServer},
				Username:   p.config.Username,
				Credential: p.config.Credential,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("could not create PeerConnection: %w", err)
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("could not add track to PeerConnection: %w", err)
	}
	go p.processRTCP(rtpSender)

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		p.candidatesMux.Lock()
		defer p.candidatesMux.Unlock()

		desc := peerConnection.RemoteDescription()
		if desc == nil {
			p.pendingCandidates = append(p.pendingCandidates, c)
			return
		}
		if err = p.sendCandidate(c); err != nil {
			p.logger.Err(err).Msg("could not send candidate")
		}
		p.logger.Debug().Msg("sent an ICEcandidate")
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		p.logger.Debug().Str("state", connectionState.String()).Msg("connection state has changed")

		if connectionState == webrtc.ICEConnectionStateFailed {
			if err = peerConnection.Close(); err != nil {
				p.logger.Panic().Err(err).Msg("closing PeerConnection")
			}
			p.logger.Info().Msg("PeerConnection has been closed")
		}
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("could not create offer: %w", err)
	}

	if err = peerConnection.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("could not set local description: %w", err)
	}

	if err := p.sendOffer(peerConnection.LocalDescription()); err != nil {
		return fmt.Errorf("could not send offer: %w", err)
	}
	p.logger.Debug().Msg("sent local description offer")

	// TODO: Timeout channel receiving to avoid blocking.
	answer := <-p.recvAnswer()
	if answer == nil {
		return nil
	}
	if err := peerConnection.SetRemoteDescription(*answer); err != nil {
		return fmt.Errorf("could not set remote description: %w", err)
	}
	p.logger.Debug().Msg("received remote answer from cloud")

	// Signal candidate after setting remote description.
	go p.signalCandidate(peerConnection)

	// Signal candidate
	p.candidatesMux.Lock()
	defer p.candidatesMux.Unlock()

	for _, c := range p.pendingCandidates {
		if err := p.sendCandidate(c); err != nil {
			return fmt.Errorf("could not send candidate: %w", err)
		}
		p.logger.Debug().Msg("sent an ICEcandidate")
	}

	return nil
}

func (p *publisher) signalCandidate(peerConnection *webrtc.PeerConnection) {
	// TODO: Stop adding ICE candidate when after signaling succeeded, that is, to exit the loop.
	// Just set a timer is not enough.
	ch := p.recvCandidate()
	for c := range ch {
		if err := peerConnection.AddICECandidate(webrtc.ICECandidateInit{
			Candidate: c,
		}); err != nil {
			p.logger.Err(err).Msg("could not add ICE candidate")
		}
		p.logger.Debug().Msg("successfully added an ICE candidate")
	}
}

// processRTCP reads incoming RTCP packets
// Before these packets are returned they are processed by interceptors.
// For things like NACK this needs to be called.
func (p *publisher) processRTCP(rtpSender *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	for {
		if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
			if errors.Is(rtcpErr, io.EOF) || errors.Is(rtcpErr, io.ErrClosedPipe) {
				_ = rtpSender.Stop()
			} else {
				p.logger.Err(rtcpErr).Send()
			}
			return
		}
	}
}

// videoTrackRTP creates a RTP video track.
// The default MIME type is H.264
func videoTrackRTP() (webrtc.TrackLocal, error) {
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video_drone",
		"edge_drone",
	)
	if err != nil {
		return nil, fmt.Errorf("could not create TrackLocalStaticRTP: %w", err)
	}
	return videoTrack, nil
}

// videoTrackSample creates a sample video track.
// The default MIME type is H.264
func videoTrackSample() (webrtc.TrackLocal, error) {
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video_webcam",
		"edge_webcam",
	)
	if err != nil {
		return nil, fmt.Errorf("could not create TrackLocalStaticSample: %w", err)
	}
	return videoTrack, nil
}
