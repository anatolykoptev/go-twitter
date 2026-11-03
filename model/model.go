// Package model holds the response types returned by the go-twitter client.
// The root package re-exports each of them as a type alias, so importing this
// package directly is optional.
package model

import (
	"errors"
	"time"
)

// ErrNotFound marks deterministic "the resource does not exist" outcomes —
// deleted tweets, suspended/unavailable handles. Pool callers treat it as a
// healthy-account response (no retry, no auth_error report): the credential
// worked; the thing asked for simply isn't there. The root package re-exports
// it as twitter.ErrNotFound.
var ErrNotFound = errors.New("twitter: resource not found")

// TwitterUser represents a Twitter/X account profile.
type TwitterUser struct {
	ID          string
	Handle      string
	DisplayName string
	Bio         string
	Followers   int
	Following   int
	TweetCount  int
	ListedCount int
	CreatedAt   time.Time
	IsVerified  bool
	HasAvatar   bool
	HasBio      bool
}

// Tweet represents a single tweet.
type Tweet struct {
	ID            string
	AuthorID      string
	AuthorHandle  string // @screen_name (from core.user_results)
	AuthorName    string // display name (from core.user_results)
	Text          string
	CreatedAt     time.Time
	Views         int
	Likes         int
	Retweets      int
	Quotes        int
	ReplyCount    int
	TokenMentions []string // extracted $TICKER patterns, e.g. ["BTC", "ETH"]

	// Promoted is true for an entry X marks as promoted (an ad), so a timeline
	// reader can skip it.
	Promoted bool

	// Pinned is true for the tweet X delivers as the profile's pinned entry
	// (TimelinePinEntry). A pinned tweet is typically old: a reader that treats
	// every tweet it has not seen as new must not treat it as a fresh post.
	Pinned bool

	// InReplyToID is the id of the tweet this one replies to, "" for a
	// standalone tweet.
	InReplyToID string

	// QuotedID is the id of the tweet this one quotes, "" when it quotes none.
	QuotedID string

	// RetweetOfID is the id of the original tweet when this entry is a
	// retweet, "" otherwise. A retweet carries the retweeter as its author;
	// the original is in RetweetOf when X delivered it in a decodable shape
	// (a deleted or unavailable original leaves RetweetOf nil but keeps the
	// id).
	RetweetOfID string

	// RetweetOf is the decoded original of a retweet, nil when this entry is
	// not a retweet or the original was unavailable.
	RetweetOf *Tweet

	// EditOf is the id of the first version when this tweet is a later edit of
	// it, "" for a tweet that was never edited (or is the first version). X
	// gives every edit its own id; EditOf lets a reader tell an edit from a
	// new post.
	EditOf string
}

// DMConversation is a single direct-message conversation from the 1.1 inbox.
// Participants holds the user IDs in the thread (the inbox shape carries
// participant IDs, not full user objects). Messages are the messages seen in the
// inbox_initial_state entries for this conversation.
type DMConversation struct {
	ConversationID string
	Participants   []string
	Messages       []DMMessage
}

// DMMessage is a single direct message. Fields mirror the 1.1 inbox
// message_data shape (id, sender_id, text, time as unix millis).
type DMMessage struct {
	ID             string
	ConversationID string
	SenderID       string
	Text           string
	CreatedAt      time.Time
}
