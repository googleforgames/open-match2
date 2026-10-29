// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mmfauth

import (
	"context"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TokenSourceFactory creates an oauth2.TokenSource for the given target audience.
type TokenSourceFactory func(ctx context.Context, audience string) (oauth2.TokenSource, error)

type audienceTokenEntry struct {
	source oauth2.TokenSource
	token  *oauth2.Token
}

// IDTokenCache caches OIDC token sources and tokens per target audience safely across concurrent callers.
type IDTokenCache struct {
	mu      sync.Mutex
	factory TokenSourceFactory
	entries map[string]*audienceTokenEntry
}

// NewIDTokenCache returns an initialized IDTokenCache backed by the given TokenSourceFactory.
func NewIDTokenCache(factory TokenSourceFactory) *IDTokenCache {
	return &IDTokenCache{
		factory: factory,
		entries: make(map[string]*audienceTokenEntry),
	}
}

// Token returns a valid OIDC token for the specified audience, reusing a cached token or source when possible.
func (c *IDTokenCache) Token(audience string) (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[audience]; ok && entry != nil {
		if entry.token != nil && entry.token.Valid() {
			return entry.token, nil
		}
		if entry.source != nil {
			if tok, err := entry.source.Token(); err == nil && tok != nil && tok.Valid() {
				entry.token = tok
				return tok, nil
			}
		}
		delete(c.entries, audience)
	}

	return c.createEntryLocked(audience)
}

// Refresh evicts a rejected access token for the specified audience and mints a replacement token,
// deduplicating concurrent refresh calls that observed the same rejected token.
func (c *IDTokenCache) Refresh(audience string, rejectedAccessToken string) (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[audience]; ok && entry != nil {
		if entry.token != nil && entry.token.AccessToken != rejectedAccessToken && entry.token.Valid() {
			return entry.token, nil
		}
		delete(c.entries, audience)
	}

	return c.createEntryLocked(audience)
}

func (c *IDTokenCache) createEntryLocked(audience string) (*oauth2.Token, error) {
	src, err := c.factory(context.Background(), audience)
	if err != nil {
		return nil, err
	}
	tok, err := src.Token()
	if err != nil || tok == nil || !tok.Valid() {
		if err == nil {
			err = status.Error(codes.Internal, "token source returned invalid token")
		}
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[string]*audienceTokenEntry)
	}
	c.entries[audience] = &audienceTokenEntry{
		source: src,
		token:  tok,
	}
	return tok, nil
}

// IsAuthRejection reports whether err represents a gRPC Unauthenticated or PermissionDenied error.
func IsAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	code := status.Code(err)
	return code == codes.Unauthenticated || code == codes.PermissionDenied
}
