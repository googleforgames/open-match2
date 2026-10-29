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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeTokenSource struct {
	mu    sync.Mutex
	calls atomic.Int32
	fn    func(call int32) (*oauth2.Token, error)
}

func (f *fakeTokenSource) Token() (*oauth2.Token, error) {
	callNum := f.calls.Add(1)
	f.mu.Lock()
	fn := f.fn
	f.mu.Unlock()
	return fn(callNum)
}

func TestIDTokenCache_ConcurrentValidTokenCaching(t *testing.T) {
	const numGoroutines = 64
	audiences := []string{
		"https://mmf-alpha.run.app",
		"https://mmf-beta.run.app",
	}

	var factoryCallsA, factoryCallsB atomic.Int32
	var sourceCallsA, sourceCallsB atomic.Int32

	cache := NewIDTokenCache(func(_ context.Context, audience string) (oauth2.TokenSource, error) {
		switch audience {
		case audiences[0]:
			factoryCallsA.Add(1)
		case audiences[1]:
			factoryCallsB.Add(1)
		default:
			return nil, fmt.Errorf("unexpected audience %q", audience)
		}

		return &fakeTokenSource{
			fn: func(_ int32) (*oauth2.Token, error) {
				if audience == audiences[0] {
					sourceCallsA.Add(1)
				} else {
					sourceCallsB.Add(1)
				}
				return &oauth2.Token{
					AccessToken: fmt.Sprintf("token-for-%s", audience),
					Expiry:      time.Now().Add(time.Hour),
				}, nil
			},
		}, nil
	})

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		aud := audiences[i%len(audiences)]
		go func(targetAudience string) {
			defer wg.Done()
			<-startBarrier
			tok, err := cache.Token(targetAudience)
			require.NoError(t, err)
			require.NotNil(t, tok)
			assert.Equal(t, fmt.Sprintf("token-for-%s", targetAudience), tok.AccessToken)
		}(aud)
	}

	close(startBarrier)
	wg.Wait()

	assert.Equal(t, int32(1), factoryCallsA.Load(), "factory should be called once for audience A")
	assert.Equal(t, int32(1), factoryCallsB.Load(), "factory should be called once for audience B")
	assert.Equal(t, int32(1), sourceCallsA.Load(), "token source should be called once for audience A")
	assert.Equal(t, int32(1), sourceCallsB.Load(), "token source should be called once for audience B")
}

func TestIDTokenCache_ExpiredTokenRefreshAndSourceRecreation(t *testing.T) {
	const audience = "https://mmf-refresh.run.app"

	t.Run("refresh via existing TokenSource without recreating source", func(t *testing.T) {
		var factoryCount atomic.Int32
		var currentToken *oauth2.Token

		src := &fakeTokenSource{
			fn: func(call int32) (*oauth2.Token, error) {
				currentToken = &oauth2.Token{
					AccessToken: fmt.Sprintf("access-%d", call),
					Expiry:      time.Now().Add(time.Hour),
				}
				return currentToken, nil
			},
		}

		cache := NewIDTokenCache(func(_ context.Context, _ string) (oauth2.TokenSource, error) {
			factoryCount.Add(1)
			return src, nil
		})

		firstTok, err := cache.Token(audience)
		require.NoError(t, err)
		require.Equal(t, "access-1", firstTok.AccessToken)
		require.Equal(t, int32(1), factoryCount.Load())
		require.Equal(t, int32(1), src.calls.Load())

		// Expire the cached token in place so the next lookup refreshes via the cached source.
		currentToken.Expiry = time.Now().Add(-time.Minute)

		secondTok, err := cache.Token(audience)
		require.NoError(t, err)
		assert.Equal(t, "access-2", secondTok.AccessToken)
		assert.Equal(t, int32(1), factoryCount.Load(), "existing TokenSource should be reused")
		assert.Equal(t, int32(2), src.calls.Load(), "existing TokenSource.Token should be called to refresh")
	})

	t.Run("recreate TokenSource when existing source returns error or expired token", func(t *testing.T) {
		var factoryCount atomic.Int32
		var firstSourceIssuedToken *oauth2.Token

		firstSource := &fakeTokenSource{
			fn: func(call int32) (*oauth2.Token, error) {
				if call == 1 {
					firstSourceIssuedToken = &oauth2.Token{
						AccessToken: "source1-initial",
						Expiry:      time.Now().Add(time.Hour),
					}
					return firstSourceIssuedToken, nil
				}
				return nil, errors.New("cached source refresh failed")
			},
		}

		var secondSourceIssuedToken *oauth2.Token
		secondSource := &fakeTokenSource{
			fn: func(call int32) (*oauth2.Token, error) {
				if call == 1 {
					secondSourceIssuedToken = &oauth2.Token{
						AccessToken: "source2-initial",
						Expiry:      time.Now().Add(time.Hour),
					}
					return secondSourceIssuedToken, nil
				}
				// Return an already-expired token on refresh so cache evicts and recreates again.
				return &oauth2.Token{
					AccessToken: "source2-expired",
					Expiry:      time.Now().Add(-time.Minute),
				}, nil
			},
		}

		thirdSource := &fakeTokenSource{
			fn: func(_ int32) (*oauth2.Token, error) {
				return &oauth2.Token{
					AccessToken: "source3-initial",
					Expiry:      time.Now().Add(time.Hour),
				}, nil
			},
		}

		cache := NewIDTokenCache(func(_ context.Context, _ string) (oauth2.TokenSource, error) {
			switch factoryCount.Add(1) {
			case 1:
				return firstSource, nil
			case 2:
				return secondSource, nil
			default:
				return thirdSource, nil
			}
		})

		tok1, err := cache.Token(audience)
		require.NoError(t, err)
		require.Equal(t, "source1-initial", tok1.AccessToken)
		require.Equal(t, int32(1), factoryCount.Load())

		// Expire token 1; firstSource fails on refresh, forcing factory recreation to secondSource.
		firstSourceIssuedToken.Expiry = time.Now().Add(-time.Minute)
		tok2, err := cache.Token(audience)
		require.NoError(t, err)
		assert.Equal(t, "source2-initial", tok2.AccessToken)
		assert.Equal(t, int32(2), factoryCount.Load())

		// Expire token 2; secondSource returns an already-expired token, forcing factory recreation to thirdSource.
		secondSourceIssuedToken.Expiry = time.Now().Add(-time.Minute)
		tok3, err := cache.Token(audience)
		require.NoError(t, err)
		assert.Equal(t, "source3-initial", tok3.AccessToken)
		assert.Equal(t, int32(3), factoryCount.Load())
	})
}

func TestIDTokenCache_RefreshSingleFlightDeduplication(t *testing.T) {
	const audience = "https://mmf-dedup.run.app"
	const numGoroutines = 16

	var factoryCount atomic.Int32
	cache := NewIDTokenCache(func(_ context.Context, _ string) (oauth2.TokenSource, error) {
		attempt := factoryCount.Add(1)
		return &fakeTokenSource{
			fn: func(_ int32) (*oauth2.Token, error) {
				return &oauth2.Token{
					AccessToken: fmt.Sprintf("minted-token-%d", attempt),
					Expiry:      time.Now().Add(time.Hour),
				}, nil
			},
		}, nil
	})

	initialTok, err := cache.Token(audience)
	require.NoError(t, err)
	require.Equal(t, "minted-token-1", initialTok.AccessToken)
	require.Equal(t, int32(1), factoryCount.Load())

	startBarrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			<-startBarrier
			refreshed, refreshErr := cache.Refresh(audience, initialTok.AccessToken)
			require.NoError(t, refreshErr)
			require.NotNil(t, refreshed)
			assert.Equal(t, "minted-token-2", refreshed.AccessToken)
		}()
	}

	close(startBarrier)
	wg.Wait()

	assert.Equal(t, int32(2), factoryCount.Load(), "factory should only be invoked once for the replacement token")
}

func TestIDTokenCache_ErrorNonCaching(t *testing.T) {
	const audience = "https://mmf-flaky.run.app"

	var factoryCalls atomic.Int32
	cache := NewIDTokenCache(func(_ context.Context, _ string) (oauth2.TokenSource, error) {
		switch factoryCalls.Add(1) {
		case 1:
			return nil, errors.New("metadata server unavailable")
		case 2:
			return &fakeTokenSource{
				fn: func(_ int32) (*oauth2.Token, error) {
					return nil, errors.New("initial token fetch error")
				},
			}, nil
		default:
			return &fakeTokenSource{
				fn: func(_ int32) (*oauth2.Token, error) {
					return &oauth2.Token{
						AccessToken: "recovered-token",
						Expiry:      time.Now().Add(time.Hour),
					}, nil
				},
			}, nil
		}
	})

	// 1. Factory error is returned and not cached.
	tok, err := cache.Token(audience)
	require.ErrorContains(t, err, "metadata server unavailable")
	assert.Nil(t, tok)

	// 2. Initial Token() error on newly minted source is returned and not cached.
	tok, err = cache.Token(audience)
	require.ErrorContains(t, err, "initial token fetch error")
	assert.Nil(t, tok)

	// 3. Subsequent call succeeds once factory and source recover.
	tok, err = cache.Token(audience)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, "recovered-token", tok.AccessToken)
	assert.Equal(t, int32(3), factoryCalls.Load())
}

func TestIsAuthRejection(t *testing.T) {
	assert.False(t, IsAuthRejection(nil))
	assert.False(t, IsAuthRejection(errors.New("plain non-grpc error")))
	assert.False(t, IsAuthRejection(status.Error(codes.OK, "ok")))
	assert.False(t, IsAuthRejection(status.Error(codes.Internal, "internal error")))
	assert.False(t, IsAuthRejection(status.Error(codes.Unavailable, "unavailable")))
	assert.False(t, IsAuthRejection(status.Error(codes.InvalidArgument, "invalid argument")))

	assert.True(t, IsAuthRejection(status.Error(codes.Unauthenticated, "invalid token")))
	assert.True(t, IsAuthRejection(status.Error(codes.PermissionDenied, "caller does not have permission")))
}
