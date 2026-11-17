package crowdsec

import (
	"errors"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

const onlineCredentialsPath = "/etc/crowdsec/online_api_credentials.yaml"

type capiCredentials struct {
	Login    string `yaml:"login"`
	Password string `yaml:"password"`
	URL      string `yaml:"url"`
}

func validateCAPICredentials(raw []byte) error {
	var credentials capiCredentials
	if err := yaml.Unmarshal(raw, &credentials); err != nil {
		return errors.New("CAPI registration returned invalid credentials YAML")
	}
	parsed, err := url.Parse(credentials.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.crowdsec.net" || parsed.User != nil || strings.TrimSpace(credentials.Login) == "" || strings.TrimSpace(credentials.Password) == "" {
		return errors.New("CAPI registration did not return complete credentials and a valid Central API URL")
	}
	return nil
}

func capiMapping(raw []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &out); err != nil {
			return nil, errors.New("CrowdSec configuration is not a YAML mapping")
		}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func mergeCAPIMaps(base, overlay map[string]any) {
	for key, value := range overlay {
		if nested, ok := value.(map[string]any); ok {
			if current, ok := base[key].(map[string]any); ok {
				mergeCAPIMaps(current, nested)
				continue
			}
		}
		base[key] = value
	}
}

func setCAPIStanza(config map[string]any, enable bool) {
	api, ok := config["api"].(map[string]any)
	if !ok {
		api = map[string]any{}
		config["api"] = api
	}
	server, ok := api["server"].(map[string]any)
	if !ok {
		server = map[string]any{}
		api["server"] = server
	}
	if enable {
		online, ok := server["online_client"].(map[string]any)
		if !ok {
			online = map[string]any{}
		}
		online["credentials_path"] = onlineCredentialsPath
		server["online_client"] = online
	} else {
		delete(server, "online_client")
	}
}

func registrationConfig(base, local []byte) ([]byte, error) {
	config, err := capiMapping(base)
	if err != nil {
		return nil, err
	}
	override, err := capiMapping(local)
	if err != nil {
		return nil, err
	}
	mergeCAPIMaps(config, override)
	setCAPIStanza(config, true)
	return yaml.Marshal(config)
}

func localCAPIConfig(local []byte, enable bool) ([]byte, error) {
	config, err := capiMapping(local)
	if err != nil {
		return nil, err
	}
	setCAPIStanza(config, enable)
	return yaml.Marshal(config)
}
