package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
)

type Config struct {
	TLSCertPath   string
	TLSKeyPath    string
	Ed25519PubKey ed25519.PublicKey
	BaseDomain    string // e.g. "challenges.cybericebox.com"
	ListenHTTPS   string // default ":443"
	CookieName    string // default "challenge"
	UDPListenAddr string // default ":51820"
}

func loadConfig() (*Config, error) {
	certPath := os.Getenv("TLS_CERT_PATH")
	keyPath := os.Getenv("TLS_KEY_PATH")
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("TLS_CERT_PATH and TLS_KEY_PATH required")
	}
	pubKeyB64 := os.Getenv("ED25519_PUBLIC_KEY")
	if pubKeyB64 == "" {
		return nil, fmt.Errorf("ED25519_PUBLIC_KEY required")
	}
	pubKeyBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode ED25519_PUBLIC_KEY: %w", err)
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ED25519_PUBLIC_KEY must be %d bytes, got %d", ed25519.PublicKeySize, len(pubKeyBytes))
	}
	baseDomain := os.Getenv("BASE_DOMAIN")
	if baseDomain == "" {
		return nil, fmt.Errorf("BASE_DOMAIN required")
	}
	listen := os.Getenv("LISTEN_HTTPS")
	if listen == "" {
		listen = ":443"
	}
	udpAddr := os.Getenv("UDP_LISTEN_ADDR")
	if udpAddr == "" {
		udpAddr = ":51820"
	}
	return &Config{
		TLSCertPath:   certPath,
		TLSKeyPath:    keyPath,
		Ed25519PubKey: ed25519.PublicKey(pubKeyBytes),
		BaseDomain:    baseDomain,
		ListenHTTPS:   listen,
		CookieName:    "challenge",
		UDPListenAddr: udpAddr,
	}, nil
}
