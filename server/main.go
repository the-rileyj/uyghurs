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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/fatih/structs"
	"github.com/gin-gonic/gin"
	"github.com/mitchellh/mapstructure"
	"github.com/the-rileyj/uyghurs"
	"gopkg.in/olahol/melody.v1"
	"gopkg.in/yaml.v2"
)

type projectMetadataHandler struct {
	projectsMetadataMap map[string]*uyghurs.ProjectMetadata
	lock                *sync.Mutex
	routerConnection    *melody.Session
}

func newProjectMetadataHandler(baseDir string) *projectMetadataHandler {
	projectsMetadataMap := getProjectsMetadataMap("apps/")

	return &projectMetadataHandler{
		lock:                &sync.Mutex{},
		projectsMetadataMap: projectsMetadataMap,
	}
}

func (pMH *projectMetadataHandler) getAllProjectsMetadata() []*uyghurs.ProjectMetadata {
	pMH.lock.Lock()

	defer pMH.lock.Unlock()

	projectsMetadata := make([]*uyghurs.ProjectMetadata, 0)

	for _, projectMetadata := range pMH.projectsMetadataMap {
		projectsMetadata = append(projectsMetadata, projectMetadata)
	}

	return projectsMetadata
}

func (pMH *projectMetadataHandler) updateProjectMetadata(projectMetadata *uyghurs.ProjectMetadata) {
	pMH.lock.Lock()

	pMH.projectsMetadataMap[projectMetadata.ProjectName] = projectMetadata

	pMH.lock.Unlock()

	if pMH.routerConnection != nil {
		routerUpdateBytes, err := json.MarshalIndent([]*uyghurs.ProjectMetadata{projectMetadata}, "", "    ")

		if err != nil {
			log.Println("error occurred marshalling JSON for router:", err)
		}

		err = pMH.routerConnection.Write(routerUpdateBytes)

		if err != nil {
			log.Println("error occurred writing JSON for router:", err)
		}
	}
}

func getProjectsMetadataMap(baseDir string) map[string]*uyghurs.ProjectMetadata {
	projectsMetadataMap := make(map[string]*uyghurs.ProjectMetadata, 0)

	err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		windowsProjectDirSplit := strings.Split(path, "\\")
		linuxProjectDirSplit := strings.Split(path, "/")

		projectDirSplit := windowsProjectDirSplit

		if len(windowsProjectDirSplit) == 1 {
			projectDirSplit = linuxProjectDirSplit
		}

		if len(projectDirSplit) == 2 && info.IsDir() {
			dockerComposePath := filepath.Join(path, "docker-compose.yml")

			if _, fileErr := os.Stat(dockerComposePath); os.IsNotExist(fileErr) {
				return nil
			}

			dockerComposeBytes, fileErr := ioutil.ReadFile(dockerComposePath)

			if fileErr != nil {
				return fileErr
			}

			var hongKongSettings uyghurs.HongKongSettings

			fileErr = yaml.Unmarshal(dockerComposeBytes, &hongKongSettings)

			if fileErr != nil {
				return fileErr
			}

			hongKongSettings.HongKongProjectSettings.ProjectName = filepath.Base(path)

			projectsMetadataMap[filepath.Base(path)] = &hongKongSettings.HongKongProjectSettings
		}

		return nil
	})

	if err != nil {
		panic(err)
	}

	return projectsMetadataMap
}

func main() {
	development := flag.Bool("d", false, "development flag")

	port := flag.Int("p", 8443, "port to run on")

	flag.Parse()

	///

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

	///

	uyghursSecretJSONFile, err := os.Open("secrets/uyghurs.json")

	if err != nil {
		panic(err)
	}

	var uyghursSecrets uyghurs.UyghursSecrets

	err = json.NewDecoder(uyghursSecretJSONFile).Decode(&uyghursSecrets)

	if err != nil {
		panic(err)
	}

	///

	routerSecretJSONFile, err := os.Open("secrets/router.json")

	if err != nil {
		panic(err)
	}

	routerSecretsJSONData := struct {
		Secret string `json:"secret"`
	}{}

	err = json.NewDecoder(routerSecretJSONFile).Decode(&routerSecretsJSONData)

	if err != nil {
		panic(err)
	}

	///

	projectsMetadata := newProjectMetadataHandler("apps/")

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
	routerWebsocketHandler := melody.New()

	workerWebsocketHandler.Config.MaxMessageSize = 10240

	var workerConnection *melody.Session

	server.GET("/worker/:uyghursSecret", func(c *gin.Context) {
		uyghursSecretsString := c.Param("uyghursSecret")

		if uyghursSecretsString != uyghursSecrets.UyghursKey {
			fmt.Println("bad worker connection request, aborting...")

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

		fmt.Println("Worker disconnected!")
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
				var messageData uyghurs.WorkResponse

				err := mapstructure.Decode(workerMessage.MessageData, &messageData)

				if err != nil {
					fmt.Println("Error parsing worker work response:", err, string(msg))

					return
				}

				if messageData.Err != "" {
					fmt.Println("Error with worker work response:", messageData.Err)

					return
				}

				fmt.Println("Received WorkResponse")

				for _, hongKongBuildSetting := range messageData.ProjectMetadata.BuildsInfo {
					timeoutContext, cancel := context.WithTimeout(context.Background(), time.Minute)

					type pullResponse struct {
						err error
					}

					responseChan := make(chan pullResponse)

					go func() {
						imagePullResponse, err := cli.ImagePull(
							timeoutContext,
							fmt.Sprintf("docker.io/therileyjohnson/%s_%s:latest", messageData.GithubData.Repository.Name, hongKongBuildSetting.Name),
							types.ImagePullOptions{
								All: true,
							},
						)

						io.Copy(ioutil.Discard, imagePullResponse)

						responseChan <- pullResponse{err}
					}()

					var pushErr error

					select {
					case <-timeoutContext.Done():
						pushErr = timeoutContext.Err()

						if pushErr != nil {
							fmt.Println("pulling image timed out")
						}
					case responseInfo := <-responseChan:
						pushErr = responseInfo.err
					}

					cancel()

					if pushErr != nil {
						fmt.Println("error pulling image:", pushErr)

						return
					}

					fmt.Println("pulled image successfully")
				}

				workingDir, err := os.Getwd()

				if err != nil {
					fmt.Println("error getting current working dir:", err)

					return
				}

				appWorkingDir := path.Join(workingDir, fmt.Sprintf("apps/%s", messageData.GithubData.Repository.Name))

				// appRepo, err := git.PlainOpen(appWorkingDir)

				// if err != nil {
				// 	fmt.Println("error opening dir for git:", err)

				// 	return
				// }

				// appRepo.Fetch(&git.FetchOptions{})

				// if err != nil {
				// 	fmt.Println("error pulling origin for app git repo:", err)

				// 	return
				// }

				// appRepoHead, err := appRepo.Head()

				// if err != nil {
				// 	fmt.Println("error getting HEAD for app git repo:", err)

				// 	return
				// }

				// appWorkTree, err := appRepo.Worktree()

				// if err != nil {
				// 	fmt.Println("error getting working tree for app git repo:", err)

				// 	return
				// }

				// appWorkTree.Reset(&git.ResetOptions{
				// 	Commit: appRepoHead.Hash(),
				// 	Mode:   git.HardReset,
				// })

				// if err != nil {
				// 	fmt.Println("error pulling origin for app git repo:", err)

				// 	return
				// }

				gitPullCommand := exec.Command("git", "pull")

				gitPullCommand.Dir = appWorkingDir

				err = gitPullCommand.Run()

				if err != nil {
					fmt.Println("error pulling app repo:", err)

					return
				}

				dockerComposeCommand := exec.Command("docker-compose", "up", "-d")

				dockerComposeCommand.Dir = appWorkingDir

				err = dockerComposeCommand.Run()

				if err != nil {
					fmt.Println("error running docker-compose:", err)

					return
				}

				fmt.Println("brought up docker-compose for:", messageData.GithubData.Repository.Name)

				projectsMetadata.updateProjectMetadata(&messageData.ProjectMetadata)

				fmt.Printf("notified RJserver of route changes for %s\n", messageData.GithubData.Repository.Name)
			case uyghurs.PingResponseType:
				fmt.Println("Received PingResponse")
			default:
				fmt.Println("Unknown worker message type:", workerMessage.Type)

				return
			}
		}
	})

	routerWebsocketHandler.HandleConnect(func(s *melody.Session) {
		if projectsMetadata.routerConnection != nil {
			// Unknown connector, ignore
			s.Close()

			return
		}

		projectsMetadata.routerConnection = s

		fmt.Println("Router connected!")

		routerRequestBytes, err := json.MarshalIndent(projectsMetadata.getAllProjectsMetadata(), "", "    ")

		if err != nil {
			log.Println("error occurred marshalling JSON for router:", err)
		}

		err = s.Write(routerRequestBytes)

		if err != nil {
			log.Println("error occurred writing JSON for router:", err)
		}

		log.Println("Sent route info to router!")
	})

	routerWebsocketHandler.HandleDisconnect(func(s *melody.Session) {
		projectsMetadata.routerConnection = nil

		fmt.Println("Router disconnected!")
	})

	server.GET("/router/:routerSecret", func(c *gin.Context) {
		routerSecretString := c.Param("routerSecret")

		if routerSecretString != routerSecretsJSONData.Secret {
			fmt.Println("bad router connection request, aborting...")

			c.AbortWithStatus(http.StatusInternalServerError)

			return
		}

		routerWebsocketHandler.HandleRequest(c.Writer, c.Request)
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
				MessageData: structs.Map(uyghurs.WorkRequest{
					GithubData: githubPush,
				}),
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
		devPort := *port

		if devPort == 443 {
			devPort = 80
		}

		server.Run(fmt.Sprintf(":%d", devPort))
	} else {
		server.RunTLS(fmt.Sprintf(":%d", *port), "secrets/RJcert.crt", "secrets/RJsecret.key")
	}
}
