package twitter_test

import (
	"context"
	"fmt"
	"log"
	"os"

	twitter "github.com/anatolykoptev/go-twitter"
)

// This example has no Output comment, so it is compiled but not run: it needs a
// real session.
func ExampleNewClient() {
	client, err := twitter.NewClient(twitter.ClientConfig{
		Accounts: []*twitter.Account{{
			Username:  "your_handle",
			AuthToken: os.Getenv("X_AUTH_TOKEN"),
			CT0:       os.Getenv("X_CT0"),
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	tweets, err := client.SearchTweets(context.Background(), "golang", twitter.SearchLatest, 20)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tweets {
		fmt.Printf("@%s: %s\n", t.AuthorHandle, t.Text)
	}
}
