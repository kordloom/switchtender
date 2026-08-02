package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pool is one set of relay workers that share a token, and the queues that token may lease from.
//
// A queue routes work to the segment that can reach it, so the queues a token may claim are the
// blast radius of that token. With one token for every worker, the least trusted worker in the
// estate could lease from the most trusted queue: compromise the machine in the DMZ and it claims a
// production run and executes it with production credentials. Binding queues to the token is what
// makes a queue a boundary rather than a routing hint.
type Pool struct {
	// Name identifies the pool in logs and in a refusal, so an operator can tell which token was
	// presented without the token appearing anywhere.
	Name string `yaml:"name" json:"name"`
	// TokenSHA256 is the hex SHA-256 of the pool's bearer token. The token itself is never stored,
	// for the same reason a webhook secret is not.
	TokenSHA256 string `yaml:"token_sha256" json:"token_sha256"`
	// Queues are the queues this pool may lease from. Empty means every queue, which is the shape a
	// single-pool install has and the only way to say "this pool is not confined".
	Queues []string `yaml:"queues,omitempty" json:"queues,omitempty"`
}

// poolDoc is the shape of a worker pool file. The wrapper exists so the file can gain other
// top-level keys later without breaking every file already written against it.
type poolDoc struct {
	// Workers are the pools the file declares.
	Workers []Pool `yaml:"workers" json:"workers"`
}

// Pools is a set of worker pools resolved by presented token.
type Pools struct {
	// pools is the declared set, in file order.
	pools []Pool
}

// LoadPools reads a worker pool file and returns the pools it declares.
//
// It fails rather than degrading to no pools. A malformed file that quietly meant "no worker may
// connect" would look like a network problem, and one that quietly meant "every worker may claim
// everything" would silently undo the confinement the file exists to express.
func LoadPools(path string) (*Pools, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("worker pool file: %w", err)
	}
	var doc poolDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("worker pool file %s: %w", path, err)
	}
	if len(doc.Workers) == 0 {
		return nil, fmt.Errorf("worker pool file %s declares no workers", path)
	}
	seen := make(map[string]struct{}, len(doc.Workers))
	for i, p := range doc.Workers {
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("worker pool file %s: pool %d has no name", path, i)
		case p.TokenSHA256 == "":
			return nil, fmt.Errorf("worker pool file %s: pool %q has no token_sha256", path, p.Name)
		}
		digest := strings.ToLower(strings.TrimSpace(p.TokenSHA256))
		if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
			return nil, fmt.Errorf("worker pool file %s: pool %q token_sha256 is not a SHA-256 "+
				"hex digest; store the hash of the token, never the token", path, p.Name)
		}
		if _, dup := seen[digest]; dup {
			return nil, fmt.Errorf("worker pool file %s: pool %q reuses another pool's token, so "+
				"the two cannot be told apart", path, p.Name)
		}
		seen[digest] = struct{}{}
		doc.Workers[i].TokenSHA256 = digest
	}
	return &Pools{pools: doc.Workers}, nil
}

// SinglePool returns the pools for an install configured with one worker token and no confinement.
// Its queue list is empty, which means every queue, and that is stated rather than implied.
func SinglePool(token string) *Pools {
	return &Pools{pools: []Pool{{Name: "default", TokenSHA256: HashToken(token)}}}
}

// HashToken returns the hex SHA-256 of a worker token, which is what a pool file stores.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// resolve returns the pool a presented token belongs to, or nil.
//
// Every pool is compared even after a match, and each comparison is constant time, so the work done
// does not depend on which token was presented or whether one matched at all.
func (p *Pools) resolve(presented string) *Pool {
	if p == nil || presented == "" {
		return nil
	}
	want := HashToken(presented)
	var found *Pool
	for i := range p.pools {
		if subtle.ConstantTimeCompare([]byte(want), []byte(p.pools[i].TokenSHA256)) == 1 {
			found = &p.pools[i]
		}
	}
	return found
}

// allows reports whether the pool may lease from every queue in want.
//
// A pool with no queues declared may lease from any of them. Asking for nothing is asking for the
// default queue, which is why an empty request is checked against the empty string rather than
// waved through: a confined pool that does not serve the default queue must not get it by omission.
func (p *Pool) allows(want []string) (string, bool) {
	if len(p.Queues) == 0 {
		return "", true
	}
	if len(want) == 0 {
		want = []string{""}
	}
	for _, q := range want {
		if !slices.Contains(p.Queues, q) {
			return q, false
		}
	}
	return "", true
}
