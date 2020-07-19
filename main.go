package main

import (
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
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

func validMAC(message, messageMAC, key []byte) bool {
	mac := hmac.New(sha1.New, key)

	mac.Write(message)

	expectedMAC := mac.Sum(nil)

	return hmac.Equal(messageMAC, expectedMAC)
}

func main() {
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

	server := gin.Default()

	development := flag.Bool("d", false, "development flag")

	flag.Parse()

	isServerErr := func(c *gin.Context, err error) bool {
		if err != nil {
			fmt.Println("ERR:", err)

			c.AbortWithStatus(http.StatusInternalServerError)

			return true
		}

		return false
	}

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

		fmt.Println(string(githubRequestPayloadBytes))
	})

	if *development {
		server.Run(":9969")
	} else {
		server.RunTLS(":9969", "secrets/RJcert.crt", "secrets/RJsecret.key")
	}
}
