package provider

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func dataSourceDockerRegistryImage() *schema.Resource {
	return &schema.Resource{
		Description: "Reads the image metadata from a Docker Registry. Used in conjunction with the [docker_image](../resources/image.md) resource to keep an image up to date on the latest available version of the tag.",

		ReadContext: dataSourceDockerRegistryImageRead,

		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Description: "The name of the Docker image, including any tags. e.g. `alpine:latest`",
				Required:    true,
			},

			"sha256_digest": {
				Type:        schema.TypeString,
				Description: "The content digest of the image, as stored in the registry.",
				Computed:    true,
			},

			"insecure_skip_verify": {
				Type:        schema.TypeBool,
				Description: "If `true`, the verification of TLS certificates of the server/registry is disabled. Defaults to `false`",
				Optional:    true,
				Default:     false,
			},
		},
	}
}

func dataSourceDockerRegistryImageRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	pullOpts := parseImageOptions(d.Get("name").(string))
	authConfig := meta.(*ProviderConfig).AuthConfigs

	// Use the official Docker Hub if a registry isn't specified
	if pullOpts.Registry == "" {
		pullOpts.Registry = "registry-1.docker.io"
	} else {
		// Otherwise, filter the registry name out of the repo name
		pullOpts.Repository = strings.Replace(pullOpts.Repository, pullOpts.Registry+"/", "", 1)
	}

	if pullOpts.Registry == "registry-1.docker.io" {
		// Docker prefixes 'library' to official images in the path; 'consul' becomes 'library/consul'
		if !strings.Contains(pullOpts.Repository, "/") {
			pullOpts.Repository = "library/" + pullOpts.Repository
		}
	}

	if pullOpts.Tag == "" {
		pullOpts.Tag = "latest"
	}

	username := ""
	password := ""

	if auth, ok := authConfig.Configs[normalizeRegistryAddress(pullOpts.Registry)]; ok {
		username = auth.Username
		password = auth.Password
	}

	insecureSkipVerify := d.Get("insecure_skip_verify").(bool)
	digest, err := getImageDigest(pullOpts.Registry, pullOpts.Repository, pullOpts.Tag, username, password, insecureSkipVerify, false)
	if err != nil {
		digest, err = getImageDigest(pullOpts.Registry, pullOpts.Repository, pullOpts.Tag, username, password, insecureSkipVerify, true)
		if err != nil {
			return diag.Errorf("Got error when attempting to fetch image version %s:%s from registry: %s", pullOpts.Repository, pullOpts.Tag, err)
		}
	}

	d.SetId(digest)
	d.Set("sha256_digest", digest)

	return nil
}

func getImageDigest(registry, image, tag, username, password string, insecureSkipVerify, fallback bool) (string, error) {
	client := http.DefaultClient
	// DevSkim: ignore DS440000
	client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify}}

	req, err := http.NewRequest("GET", "https://"+registry+"/v2/"+image+"/manifests/"+tag, nil)
	if err != nil {
		return "", fmt.Errorf("Error creating registry request: %s", err)
	}

	if username != "" {
		if registry != "ghcr.io" {
			req.SetBasicAuth(username, password)
		} else {
			req.Header.Add("Authorization", "Bearer "+b64.StdEncoding.EncodeToString([]byte(password)))
		}
	}

	// We accept schema v2 manifests and manifest lists, and also OCI types
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.list.v2+json")
	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.oci.image.index.v1+json")

	if fallback {
		// Fallback to this header if the registry does not support the v2 manifest like gcr.io
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v1+prettyjws")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Error during registry request: %s", err)
	}

	switch resp.StatusCode {
	// Basic auth was valid or not needed
	case http.StatusOK:
		return getDigestFromResponse(resp)

	// Either OAuth is required or the basic auth creds were invalid
	case http.StatusUnauthorized:
		if strings.HasPrefix(resp.Header.Get("www-authenticate"), "Bearer") {
			auth := parseAuthHeader(resp.Header.Get("www-authenticate"))
			params := url.Values{}
			params.Set("service", auth["service"])
			params.Set("scope", auth["scope"])
			tokenRequest, err := http.NewRequest("GET", auth["realm"]+"?"+params.Encode(), nil)
			if err != nil {
				return "", fmt.Errorf("Error creating registry request: %s", err)
			}

			if username != "" {
				tokenRequest.SetBasicAuth(username, password)
			}

			tokenResponse, err := client.Do(tokenRequest)
			if err != nil {
				return "", fmt.Errorf("Error during registry request: %s", err)
			}

			if tokenResponse.StatusCode != http.StatusOK {
				return "", fmt.Errorf("Got bad response from registry: " + tokenResponse.Status)
			}

			body, err := ioutil.ReadAll(tokenResponse.Body)
			if err != nil {
				return "", fmt.Errorf("Error reading response body: %s", err)
			}

			token := &TokenResponse{}
			err = json.Unmarshal(body, token)
			if err != nil {
				return "", fmt.Errorf("Error parsing OAuth token response: %s", err)
			}

			req.Header.Set("Authorization", "Bearer "+token.Token)
			digestResponse, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("Error during registry request: %s", err)
			}

			if digestResponse.StatusCode != http.StatusOK {
				return "", fmt.Errorf("Got bad response from registry: " + digestResponse.Status)
			}

			return getDigestFromResponse(digestResponse)
		}

		return "", fmt.Errorf("Bad credentials: " + resp.Status)

		// Some unexpected status was given, return an error
	default:
		return "", fmt.Errorf("Got bad response from registry: " + resp.Status)
	}
}

type TokenResponse struct {
	Token string
}

// Parses key/value pairs from a WWW-Authenticate header
func parseAuthHeader(header string) map[string]string {
	parts := strings.SplitN(header, " ", 2)
	parts = strings.Split(parts[1], ",")
	opts := make(map[string]string)

	for _, part := range parts {
		vals := strings.SplitN(part, "=", 2)
		key := vals[0]
		val := strings.Trim(vals[1], "\", ")
		opts[key] = val
	}

	return opts
}

func getDigestFromResponse(response *http.Response) (string, error) {
	header := response.Header.Get("Docker-Content-Digest")

	if header == "" {
		body, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return "", fmt.Errorf("Error reading registry response body: %s", err)
		}

		return fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
	}

	return header, nil
}
