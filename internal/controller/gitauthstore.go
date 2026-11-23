package controller

import (
	"context"
	"encoding/json"
	"fmt"

	controllerdb "github.com/semistrict/hotbed/internal/controller/db/gen"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// gitCredentialSecret is the write-only secret payload kept in git_credentials.secret.
// It is never serialized into GitCredential protobuf messages or returned by list calls.
type gitCredentialSecret struct {
	Token            string `json:"token,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	ClientID         string `json:"clientId,omitempty"`
	TokenExpiresAt   int64  `json:"tokenExpiresAt,omitempty"`   // unix ms
	RefreshExpiresAt int64  `json:"refreshExpiresAt,omitempty"` // unix ms
}

type storedGitCredential struct {
	cred   *pb.GitCredential
	secret gitCredentialSecret
}

// PutGitCredential stores or replaces a centralized git credential and its secret.
func (s *Store) PutGitCredential(ctx context.Context, cred *pb.GitCredential, secret gitCredentialSecret) error {
	doc, err := marshal(cred)
	if err != nil {
		return err
	}
	sec, err := json.Marshal(secret)
	if err != nil {
		return err
	}
	return s.q.UpsertGitCredential(ctx, controllerdb.UpsertGitCredentialParams{
		Name:   cred.Name,
		Host:   cred.Host,
		Doc:    doc,
		Secret: string(sec),
	})
}

// DeleteGitCredential removes a git credential by name.
func (s *Store) DeleteGitCredential(ctx context.Context, name string) (bool, error) {
	n, err := s.q.DeleteGitCredential(ctx, name)
	return n > 0, err
}

// GitCredentials lists stored git credentials' metadata (never their secrets).
func (s *Store) GitCredentials(ctx context.Context) ([]*pb.GitCredential, error) {
	docs, err := s.q.ListGitCredentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.GitCredential, 0, len(docs))
	for _, doc := range docs {
		c, err := unmarshal(doc, &pb.GitCredential{})
		if err != nil {
			return nil, fmt.Errorf("read git credential: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// GitCredentialsByHost returns credentials and secrets for host, for token
// minting and GitHub API proxying only.
func (s *Store) GitCredentialsByHost(ctx context.Context, host string) ([]storedGitCredential, error) {
	rows, err := s.q.ListGitCredentialsByHost(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]storedGitCredential, 0, len(rows))
	for _, r := range rows {
		c, err := unmarshal(r.Doc, &pb.GitCredential{})
		if err != nil {
			return nil, fmt.Errorf("read git credential: %w", err)
		}
		var sec gitCredentialSecret
		if err := json.Unmarshal([]byte(r.Secret), &sec); err != nil {
			return nil, fmt.Errorf("read git credential %s secret: %w", c.Name, err)
		}
		out = append(out, storedGitCredential{cred: c, secret: sec})
	}
	return out, nil
}
