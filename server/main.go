package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"github.com/the-rileyj/uyghurs"
	"gopkg.in/olahol/melody.v1"
)

func main() {
	development := flag.Bool("d", false, "development flag")

	flag.Parse()

	githubSecretJSONFile, err := os.Open("secrets/github.json")

	if err != nil {
		panic(err)
	}

	githubSecretsJSONData := struct {
		Secret string `json:"secret"`
	}{}

	err = json.NewDecoder(githubSecretJSONFile).Decode(&githubSecretsJSONData)

	if err != nil {
		panic(err)
	}

	uyghursSecretJSONFile, err := os.Open("secrets/uyghurs.json")

	if err != nil {
		panic(err)
	}

	var uyghursSecrets uyghurs.UyghursSecrets

	err = json.NewDecoder(uyghursSecretJSONFile).Decode(&uyghursSecrets)

	if err != nil {
		panic(err)
	}

	cli, err := client.NewEnvClient()

	if err != nil {
		panic(err)
	}

	server := gin.Default()

	isServerErr := func(c *gin.Context, err error) bool {
		if err != nil {
			fmt.Println("ERR:", err)

			c.AbortWithStatus(http.StatusInternalServerError)

			return true
		}

		return false
	}

	workerWebsocketHandler := melody.New()

	var workerConnection *melody.Session

	server.GET("/worker/:uyghursSecret", func(c *gin.Context) {
		uyghursSecretsString := c.Param("uyghursSecret")

		if uyghursSecretsString != uyghursSecrets.UyghursKey {
			fmt.Println("bad request, aborting...")

			c.AbortWithStatus(http.StatusInternalServerError)

			return
		}

		workerWebsocketHandler.HandleRequest(c.Writer, c.Request)
	})

	workerWebsocketHandler.HandleConnect(func(s *melody.Session) {
		if workerConnection != nil {
			// Unknown connector, ignore
			s.Close()

			return
		}

		fmt.Println("Worker connected!")

		workerConnection = s
	})

	workerWebsocketHandler.HandleDisconnect(func(s *melody.Session) {
		workerConnection = nil
	})

	workerWebsocketHandler.HandleMessage(func(s *melody.Session, msg []byte) {
		if s == workerConnection {
			var workerMessage uyghurs.WorkerMessage

			err := json.Unmarshal(msg, &workerMessage)

			if err != nil {
				fmt.Println("Error unmarshalling worker message:", err)

				return
			}

			switch uyghurs.WorkerMessageType(workerMessage.Type) {
			case uyghurs.WorkResponseType:
				messageData, ok := workerMessage.MessageData.(uyghurs.WorkResponse)

				if !ok {
					fmt.Println("Error parsing worker work response", string(msg))

					return
				}

				if messageData.Err != "" {
					fmt.Println("Error with worker work response:", messageData.Err)

					return
				}

				fmt.Println("Received WorkResponse")

				timeoutContext, cancel := context.WithTimeout(context.Background(), time.Minute)

				type pullResponse struct {
					response io.ReadCloser
					err      error
				}

				responseChan := make(chan pullResponse)

				go func() {
					response, err := cli.ImagePull(
						timeoutContext,
						fmt.Sprintf("therileyjohnson/%s:latest", messageData.GithubData.Repository.Name),
						types.ImagePullOptions{
							All: true,
						},
					)

					responseChan <- pullResponse{response, err}
				}()

				var pushErr error

				select {
				case <-timeoutContext.Done():
					pushErr = timeoutContext.Err()

					if pushErr != nil {
						fmt.Println("pulling image timed out")
					}
				case responseInfo := <-responseChan:
					if responseInfo.err != nil {
						pushErr = responseInfo.err

						responseBytes, err := ioutil.ReadAll(responseInfo.response)

						if err != nil {
							fmt.Println("error reading image pull response:", err)
						} else {
							fmt.Println(string(responseBytes))
						}
					}
				}

				cancel()

				if pushErr != nil {
					fmt.Println("error pull image:", err)

					return
				}

				fmt.Println("pulled image successfully")
			case uyghurs.PingResponseType:
				fmt.Println("Received PingResponse")
			default:
				fmt.Println("Unknown worker message type:", workerMessage.Type)

				return
			}
		}
	})

	server.POST("/", func(c *gin.Context) {
		githubRequestPayloadBytes, err := ioutil.ReadAll(c.Request.Body)

		if isServerErr(c, err) {
			return
		}

		githubRequestPayloadHeader := c.Request.Header.Get("X-Hub-Signature")

		if githubRequestPayloadHeader == "" {
			isServerErr(c, errors.New("github request payload header wrong"))

			return
		}

		githubRequestPayloadSignatureParts := strings.SplitN(githubRequestPayloadHeader, "=", 2)

		if len(githubRequestPayloadSignatureParts) != 2 {
			isServerErr(c, errors.New("error parsing signature"))

			return
		}

		var githubHashFunc func() hash.Hash

		switch githubRequestPayloadSignatureParts[0] {
		case "sha1":
			githubHashFunc = sha1.New
		case "sha256":
			githubHashFunc = sha256.New
		case "sha512":
			githubHashFunc = sha512.New
		default:
			isServerErr(c, fmt.Errorf("unknown hash type prefix: %q", githubRequestPayloadSignatureParts[0]))

			return
		}

		mac := hmac.New(githubHashFunc, []byte(githubSecretsJSONData.Secret))

		mac.Write(githubRequestPayloadBytes)

		expectedMAC := mac.Sum(nil)

		signatureBytes, err := hex.DecodeString(githubRequestPayloadSignatureParts[1])

		if isServerErr(c, err) {
			return
		}

		if !hmac.Equal(signatureBytes, expectedMAC) {
			isServerErr(c, fmt.Errorf("unequal hmacs %s != %s", string(signatureBytes), string(expectedMAC)))

			return
		}

		var githubPush uyghurs.GithubPush

		err = json.Unmarshal(githubRequestPayloadBytes, &githubPush)

		if isServerErr(c, err) {
			return
		}

		if workerConnection != nil {
			workerRequest := uyghurs.WorkerMessage{
				Type: int(uyghurs.WorkRequestType),
				MessageData: uyghurs.WorkRequest{
					GithubData: githubPush,
				},
			}

			workerRequestBytes, err := json.MarshalIndent(workerRequest, "", "    ")

			if isServerErr(c, err) {
				return
			}

			err = workerConnection.Write(workerRequestBytes)

			if isServerErr(c, err) {
				return
			}
		} else {
			fmt.Println("no worker available for request")
		}
	})

	if *development {
		server.Run(":9969")
	} else {
		server.RunTLS(":9969", "secrets/RJcert.crt", "secrets/RJsecret.key")
	}
}
