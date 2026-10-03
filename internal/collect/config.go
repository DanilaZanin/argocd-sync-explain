package collect

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Conn is the resolved Argo CD API connection.
type Conn struct {
	Server    string
	Token     string
	Insecure  bool
	PlainText bool
}

type localConfig struct {
	CurrentContext string `json:"current-context"`
	Contexts       []struct {
		Name   string `json:"name"`
		Server string `json:"server"`
		User   string `json:"user"`
	} `json:"contexts"`
	Servers []struct {
		Server    string `json:"server"`
		Insecure  bool   `json:"insecure"`
		PlainText bool   `json:"plain-text"`
	} `json:"servers"`
	Users []struct {
		Name      string `json:"name"`
		AuthToken string `json:"auth-token"`
	} `json:"users"`
}

// DefaultConfigPath is the argocd CLI config location.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "argocd", "config")
}

// ResolveConn fills Conn from flag values (non-empty win), then the environment
// (ARGOCD_SERVER, ARGOCD_AUTH_TOKEN), then the argocd CLI config file.
func ResolveConn(server, token, cfgPath, ctxName string, insecure, plainText bool, env func(string) string) (Conn, error) {
	c := Conn{Server: server, Token: token, Insecure: insecure, PlainText: plainText}
	if c.Server == "" {
		c.Server = env("ARGOCD_SERVER")
	}
	if c.Token == "" {
		c.Token = env("ARGOCD_AUTH_TOKEN")
	}
	if c.Server != "" && c.Token != "" {
		return c, nil
	}
	if cfgPath == "" {
		cfgPath = DefaultConfigPath()
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	var lc localConfig
	if err := yaml.Unmarshal(b, &lc); err != nil {
		return c, fmt.Errorf("parse %s: %w", cfgPath, err)
	}
	if ctxName == "" {
		ctxName = lc.CurrentContext
	}
	for _, cx := range lc.Contexts {
		if cx.Name != ctxName {
			continue
		}
		explicit := c.Server != ""
		// The config only describes cx.Server. A token or TLS setting from it must never be sent
		// to a different server that the user named explicitly.
		if explicit && c.Server != cx.Server {
			break
		}
		if c.Server == "" {
			c.Server = cx.Server
		}
		for _, s := range lc.Servers {
			if s.Server == cx.Server {
				c.Insecure = c.Insecure || s.Insecure
				c.PlainText = c.PlainText || s.PlainText
			}
		}
		if c.Token == "" {
			for _, u := range lc.Users {
				if u.Name == cx.User {
					c.Token = u.AuthToken
				}
			}
		}
	}
	return c, nil
}
