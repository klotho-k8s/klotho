package linode

import (
	"net/http"

	"github.com/linode/linodego/v2"
)

// NewClient builds a linodego client authenticated with the configured token.
// It makes no API calls. *linodego.Client satisfies API.
func NewClient(cfg Config) (*linodego.Client, error) {
	if cfg.Token == "" {
		return nil, errorf("a Linode API token is required (set LINODE_TOKEN)")
	}
	client, err := linodego.NewClient(&http.Client{})
	if err != nil {
		return nil, errorf("building the Linode client: %w", err)
	}
	client.SetToken(cfg.Token)
	return &client, nil
}
