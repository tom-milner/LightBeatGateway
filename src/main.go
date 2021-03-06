package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/joho/godotenv"
	"github.com/tom-milner/LightBeatGateway/edge"
	"github.com/tom-milner/LightBeatGateway/edge/topics"
	"github.com/tom-milner/LightBeatGateway/hardware"
	"github.com/tom-milner/LightBeatGateway/spotify"
	"github.com/tom-milner/LightBeatGateway/spotify/models"

	"github.com/tom-milner/LightBeatGateway/utils/colors"
)

const enableHardware bool = runtime.GOARCH == "arm"

type TriggerType string

const (
	Beat TriggerType = "beat"
	Bar  TriggerType = "bar"
)

var currentTriggerType = Beat

func init() {
	if err := godotenv.Load("../.env"); err != nil {
		log.Fatal("No .env file found.")
	}

}

func SetTriggerMessageHandler(msg edge.EdgeMessage) {
	log.Println(msg.Topic())
	log.Println(msg.Payload())
	currentTriggerType = TriggerType(msg.Payload())
}

// Setup all the various libraries/connections.
func setup() {
	// Get Spotify Environment vars.
	spotifyClientID := getRequiredEnv("SPOTIFY_CLIENT_ID")
	spotifyClientSecret := getRequiredEnv("SPOTIFY_CLIENT_SECRET")

	// Get MQTT Environment vars.
	brokerAddress := getRequiredEnv("MQTT_BROKER_ADDRESS")
	brokerPort := getRequiredEnv("MQTT_BROKER_PORT")

	log.Println("Environment variables loaded successfully.")

	// Authenticate with spotify API.
	tokenFile := "../tokens.json"
	if !spotify.Authorize(tokenFile, spotifyClientID, spotifyClientSecret) {
		log.Fatal("Failed to authorize spotify wrapper")
	}

	// Connect to MQTT broker
	broker := edge.MQTTBroker{
		Address: brokerAddress,
		Port:    brokerPort,
	}
	info := edge.MQTTConnInfo{
		ClientID: "LightBeatGateway",
		Broker:   broker,
	}
	_, err := edge.ConnectToMQTTBroker(info)
	if err != nil {
		log.Fatal(err)
	}

	// Subscribe to the relevant topics.
	edge.OnReceive(topics.SetTrigger, SetTriggerMessageHandler)

	// Setup Blinkt.
	if enableHardware {
		hardware.SetupLights()
	}
}

func main() { // Setup
	setup()
	startSpotifySync()
}

//
func startSpotifySync() {
	log.Println("Starting ticker")
	lastPlaying, _ := spotify.GetCurrentlyPlaying()
	tickerInterval := 2 * time.Second
	ticker := time.NewTicker(tickerInterval)
	var triggerContext context.Context
	var cancel context.CancelFunc
	isDetecting := false

	lastTrigger := currentTriggerType

	for {
		<-ticker.C
		currPlay, _ := spotify.GetCurrentlyPlaying()
		if currPlay.Item.ID == "" {
			continue
		}

		// TODO: Come up with a better way to detect and react to state changes.

		// Whether the media has stopped or started playing.
		changeInPlayState := lastPlaying.IsPlaying != currPlay.IsPlaying

		// Whether the playing media has changed.
		changeInMedia := lastPlaying.Item.ID != currPlay.Item.ID

		// Whether the progress of the media has been changed by more than it should've in the given time interval
		progressDelta := time.Duration(math.Abs(float64(currPlay.Progress - lastPlaying.Progress)))
		progressChanged := progressDelta > ((tickerInterval + time.Second) / time.Millisecond) // +1 second just to be sure.

		// Whether media is playing, but we aren't running the beat detector.
		playingWithoutDetection := (!isDetecting && currPlay.IsPlaying)

		// Whether the trigger type has changed.
		triggerTypeChanged := currentTriggerType != lastTrigger

		// TODO: Refactor these massive if statements!!!!
		if ((changeInPlayState && !currPlay.IsPlaying) || changeInMedia || progressChanged || (triggerTypeChanged && currPlay.IsPlaying)) && !playingWithoutDetection {
			log.Println("Stopping")
			cancel()
			isDetecting = false
		}

		if ((changeInPlayState && currPlay.IsPlaying) || changeInMedia || progressChanged) || playingWithoutDetection || (triggerTypeChanged && currPlay.IsPlaying) {
			log.Println("Starting")
			triggerContext, cancel = context.WithCancel(context.Background())

			mediaAnalysis, err := spotify.GetMediaAudioAnalysis(currPlay.Item.ID)
			if err != nil {
				continue
			}
			b, _ := json.Marshal(currPlay)
			go edge.SendMessage(topics.NewMedia, b)

			mediaFeatures, err := spotify.GetMediaAudioFeatures(currPlay.Item.ID)
			if err != nil {
				continue
			}
			b, _ = json.Marshal(mediaFeatures)
			go edge.SendMessage(topics.MediaFeatures, b)

			go startTriggerSync(triggerContext, currPlay, mediaAnalysis, currentTriggerType)
			isDetecting = true
		}

		lastPlaying = currPlay
		lastTrigger = currentTriggerType
	}
}

// Sync with the Playing spotify data.
func startTriggerSync(ctx context.Context, currPlay models.Media, mediaAnalysis models.MediaAudioAnalysis, trigger TriggerType) {
	log.Println("Tracking triggers.")

	fmt.Println(currPlay.Item.Name)
	spew.Dump((mediaAnalysis.Beats))
	spew.Dump((mediaAnalysis.Bars))
	log.Printf("Trigger: %s", trigger)

	// Calculate when to show the first trigger.

	var triggers []models.TimeInterval
	// Use the triggers specified by the user.
	switch trigger {
	case Bar:
		triggers = make([]models.TimeInterval, len(mediaAnalysis.Bars))
		triggers = mediaAnalysis.Bars
	case Beat:
		fallthrough
	default:
		log.Println("Using beat as trigger")
		triggers = make([]models.TimeInterval, len(mediaAnalysis.Beats))
		triggers = mediaAnalysis.Beats
	}
	log.Println(trigger)
	spew.Dump(triggers)

	numTriggers := len(triggers)
	var nextTrigger int = 0
	progress := time.Duration(currPlay.Progress) * time.Millisecond

	fmt.Printf("Progress: %v\n", progress)

	for i := 0; i < numTriggers; i++ {
		// Find the next trigger.
		if progress >= time.Duration((triggers[i].Start)*float64(time.Second)) {
			nextTrigger = i + 1
		}
	}
	log.Printf("Next Trigger: %d", nextTrigger)
	timeTillNextTrigger := time.Duration(triggers[nextTrigger].Start*float64(time.Second)) - progress

	fmt.Printf("Trigger: %v\n", nextTrigger)
	fmt.Printf("numTriggers: %v\n", numTriggers)

	fmt.Printf("Time till next trigger: %v\n", timeTillNextTrigger)
	ticker := time.NewTicker(timeTillNextTrigger)
	for nextTrigger < numTriggers-1 {
		select {
		case <-ticker.C:
			nextTrigger++
			triggerDuration := time.Duration((triggers[nextTrigger].Duration) * float64(time.Second))
			ticker = time.NewTicker(triggerDuration)
			go onTrigger(nextTrigger, triggerDuration)
		case <-ctx.Done():
			log.Println("Heard cancel. Exiting")
			return
		}
	}
}

// Function to run on every beat.
func onTrigger(triggerNum int, triggerDuration time.Duration) {

	// Generate json payload.
	info := &models.Trigger{
		Number:   triggerNum,
		Duration: int(triggerDuration / time.Millisecond),
	}

	message, _ := json.Marshal(info)

	// go edge.SendMessage(topics.Trigger, message)
	if enableHardware {
		hardware.FlashSequence(colors.Red, triggerDuration, triggerNum&1 != 0)
	}
	log.Println(string(message))
}

func getRequiredEnv(key string) string {
	envVar, exists := os.LookupEnv(key)
	if !exists {
		log.Fatal(key + " is required.")
	}
	return envVar
}
