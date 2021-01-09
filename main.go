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
	"github.com/tom-milner/LightBeatGateway/hardware"
	"github.com/tom-milner/LightBeatGateway/iot"
	"github.com/tom-milner/LightBeatGateway/iot/topics"
	"github.com/tom-milner/LightBeatGateway/spotify"
	"github.com/tom-milner/LightBeatGateway/spotify/models"

	//	"github.com/tom-milner/LightBeatGateway/utils"
	"github.com/tom-milner/LightBeatGateway/utils/colors"
)

const enableHardware bool = runtime.GOARCH == "arm"

func init() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("No .env file found.")
	}

}

func main() { // Setup

	// Get Spotify Environment vars.
	spotifyClientID := getRequiredEnv("SPOTIFY_CLIENT_ID")
	spotifyClientSecret := getRequiredEnv("SPOTIFY_CLIENT_SECRET")

	// Get MQTT Environment vars.
	brokerAddress := getRequiredEnv("MQTT_BROKER_ADDRESS")
	brokerPort := getRequiredEnv("MQTT_BROKER_PORT")

	log.Println("Environment variables loaded successfully.")

	// Authenticate with spotify API.
	tokenFile := "tokens.json"
	if !spotify.Authorize(tokenFile, spotifyClientID, spotifyClientSecret) {
		log.Fatal("Failed to authorize spotify wrapper")
	}

	// Connect to MQTT broker
	broker := iot.MQTTBroker{
		Address: brokerAddress,
		Port:    brokerPort,
	}
	info := iot.MQTTConnInfo{
		ClientID: "LightBeatGateway",
		Broker:   broker,
	}
	_, err := iot.ConnectToMQTTBroker(info)
	if err != nil {
		log.Fatal(err)
	}

	// Setup Blinkt.
	if enableHardware {
		hardware.SetupLights()
	}

	// Run the main program loop.
	run()
}

func run() {
	log.Println("Starting ticker")
	lastPlaying, _ := spotify.GetCurrentlyPlaying()
	tickerInterval := 2 * time.Second
	ticker := time.NewTicker(tickerInterval)
	var beatContex context.Context
	var cancel context.CancelFunc
	isDetecting := false

	for {
		<-ticker.C
		currPlay, _ := spotify.GetCurrentlyPlaying()
		if currPlay.Item.ID == "" {
			continue
		}

		// Whether the media has stopped or started playing.
		changeInPlayState := lastPlaying.IsPlaying != currPlay.IsPlaying

		// Whether the playing media has changed.
		changeInMedia := lastPlaying.Item.ID != currPlay.Item.ID

		// Whether the progress of the media has been changed by more than it should've in the given time interval
		progressDelta := time.Duration(math.Abs(float64(currPlay.Progress - lastPlaying.Progress)))
		progressChanged := progressDelta > ((tickerInterval + time.Second) / time.Millisecond) // +1 second just to be sure.

		// Whether media is playing, but we aren't running the beat detector.
		playingWithoutDetection := (!isDetecting && currPlay.IsPlaying)

		if ((changeInPlayState && !currPlay.IsPlaying) || changeInMedia || progressChanged) && !playingWithoutDetection {
			log.Println("Stopping")
			cancel()
			isDetecting = false
		}

		if ((changeInPlayState && currPlay.IsPlaying) || changeInMedia || progressChanged) || playingWithoutDetection {
			log.Println("Starting")
			beatContex, cancel = context.WithCancel(context.Background())

			mediaAnalysis, err := spotify.GetMediaAudioAnalysis(currPlay.Item.ID)
			if err != nil {
				continue
			}
			b, _ := json.Marshal(currPlay)
			go iot.SendMessage(topics.NewMedia, b)

			mediaFeatures, err := spotify.GetMediaAudioFeatures(currPlay.Item.ID)
			if err != nil {
				continue
			}
			b, _ = json.Marshal(mediaFeatures)
			go iot.SendMessage(topics.MediaFeatures, b)

			go triggerBeats(beatContex, currPlay, mediaAnalysis)
			isDetecting = true
		}

		lastPlaying = currPlay
	}
}

func triggerBeats(ctx context.Context, currPlay models.Media, mediaAnalysis models.MediaAudioAnalysis) {
	log.Println("Tracking beats.")

	fmt.Println(currPlay.Item.Name)

	// Calculate when to show the first trigger.
	triggers := mediaAnalysis.Beats
	spew.Dump(triggers[0])

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
	info := &models.Beat{
		Number:   triggerNum,
		Duration: int(triggerDuration / time.Millisecond),
	}

	message, _ := json.Marshal(info)

	go iot.SendMessage(topics.Beat, message)
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
