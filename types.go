package twitter

import "github.com/anatolykoptev/go-twitter/model"

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
