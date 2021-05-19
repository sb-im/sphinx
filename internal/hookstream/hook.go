package hookstream

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"

	mqttclient "github.com/SB-IM/mqtt-client"
	pb "github.com/SB-IM/pb/signal"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog/log"
)

type ConfigOptions struct {
	HookCommandLine
	MQTTClientConfigOptions
}

type MQTTClientConfigOptions struct {
	HookStreamTopicPrefix string
	Qos                   uint
	Retained              bool
}

type HookCommandLine struct {
	Service     string
	WaitTimeout time.Duration
}

func Exec(ctx context.Context, config ConfigOptions) (errCh chan error) {
	logger := log.Ctx(ctx)
	client := mqttclient.FromContext(ctx)

	errCh = make(chan error, 1)

	topic := config.HookStreamTopicPrefix + "/" + machineID() + "/" + strconv.Itoa(int(pb.TrackSource_DRONE))
	t := client.Subscribe(topic, byte(config.Qos), func(c mqtt.Client, _ mqtt.Message) {
		time.Sleep(config.WaitTimeout)
		logger.Info().Dur("wait", config.WaitTimeout).Msg("wait for a while")

		if err := exec.CommandContext(
			ctx,
			"systemctl",
			"restart",
			config.Service,
		).Run(); err != nil {
			errCh <- err
			return
		}
		logger.Info().Str("service", config.Service).Msg("restarted service")
	})
	go func() {
		<-t.Done()
		if t.Error() != nil {
			logger.Err(t.Error()).Msgf("could not subscribe to %s", topic)
		} else {
			logger.Info().Msgf("subscribed to %s", topic)
		}
	}()

	return
}

func machineID() string {
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		panic(err)
	}
	return string(bytes.TrimSuffix(id, []byte("\n")))
}
