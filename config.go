package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

var (
	configPath = flag.String("config", "config.json", "path to config file")
	// RestoreDNS makes the program restore DNS settings from a leftover
	// dns_backup.json and exit, without starting the proxy.
	RestoreDNS = flag.Bool("restore", false, "restore DNS settings from backup and exit")
)

type Config struct {
	Server            string   `json:"server"`
	Port              int      `json:"port"`
	User              string   `json:"user"`
	Password          string   `json:"password"`
	PrivateKeyPath    string   `json:"private_key_path"`
	ListenPort        int      `json:"listen_port"`
	TUNInterface      string   `json:"tun_interface"`
	TUNAddress        string   `json:"tun_address"`
	TUNGateway        string   `json:"tun_gateway"`
	TUNMTU            int      `json:"tun_mtu"`
	DNSServer         string   `json:"dns_server"`
	ExcludeInterfaces []string `json:"exclude_interfaces"`
	LocalSubnets      []string `json:"local_subnets"`
}

func DefaultConfig() *Config {
	return &Config{
		Server:       "192.168.3.33",
		Port:         10022,
		User:         "root",
		ListenPort:   10010,
		TUNInterface: "tun0",
		TUNAddress:   "172.19.0.1/30",
		TUNGateway:   "172.19.0.2",
		TUNMTU:       1400,
		DNSServer:         "127.0.0.1",
		ExcludeInterfaces: []string{},
		LocalSubnets: []string{
			"192.168.0.0/16",
			"10.0.0.0/8",
			"172.16.0.0/12",
			"127.0.0.0/8",
		},
	}
}

func LoadConfig() (*Config, error) {
	flag.Parse()

	cfg := DefaultConfig()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server == "" || cfg.User == "" {
		return nil, fmt.Errorf("server and user are required")
	}
	if cfg.Password == "" && cfg.PrivateKeyPath == "" {
		return nil, fmt.Errorf("password or private_key_path is required")
	}

	return cfg, nil
}
