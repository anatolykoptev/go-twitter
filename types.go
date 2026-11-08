package twitter

import (
	"time"

	"github.com/anatolykoptev/go-twitter/internal/parse"
	"github.com/anatolykoptev/go-twitter/model"
)

// The response types live in package model so the internal parsers can return
// them without importing this package; the aliases keep the names unchanged.
type (
	// TwitterUser represents a Twitter/X account profile.
	TwitterUser = model.TwitterUser
	// Tweet represents a single tweet.
	Tweet = model.Tweet
	// DMConversation is a single direct-message conversation from the 1.1 inbox.
	DMConversation = model.DMConversation
	// DMMessage is a single direct message.
	DMMessage = model.DMMessage
)

// Cursor is used for paginated GraphQL requests.
type Cursor struct {
	Value  string
	IsNext bool
}

// SearchProduct selects the SearchTimeline results tab.
type SearchProduct string

const (
	SearchTop    SearchProduct = "Top"
	SearchLatest SearchProduct = "Latest"
	SearchPeople SearchProduct = "People"
	SearchMedia  SearchProduct = "Media"
)

// SnowflakeTime returns the creation time encoded in a Twitter/X snowflake ID
// (a tweet ID): (id >> 22) + 1288834974657 ms. Consumers use it to compute
// their own publication-to-first-seen lag.
func SnowflakeTime(id string) (time.Time, error) { return parse.SnowflakeTime(id) }
