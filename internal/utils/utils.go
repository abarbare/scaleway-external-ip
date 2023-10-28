/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/pkg/errors"
)

func CommonPrefix(list []string) string {
	if len(list) == 0 {
		return ""
	}
	k := list[0]
	if len(k) == 1 {
		return k
	}
	for _, s := range list[1:] {
		lK := len(k)
		lS := len(s)
		max := lK
		if lS < max {
			max = lS
		}
		for i := 0; i < max; i++ {
			if k[i] != s[i] {
				k = k[:i]
				break
			}
		}

	}
	return k
}

func GetV4OrV664Prefix(ip string) (string, error) {
	parsedIP, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("error parsing ip %s: %w", ip, err)
	}
	if parsedIP.Is6() {
		parsedPrefix, err := parsedIP.Prefix(64)
		if err != nil {
			return "", fmt.Errorf("error getting /64 prefix for %s: %w", ip, err)
		}
		return parsedPrefix.String(), nil
	}
	return parsedIP.String(), nil
}

// TODO : add in scaleway-sdk-go
var (
	metadataURL = "http://169.254.42.42"
)

type Metadata struct {
	PublicIPsV6 []struct {
		ID      string `json:"id,omitempty"`
		Address string `json:"address,omitempty"`
		Gateway string `json:"gateway,omitempty"`
	} `json:"public_ips_v6,omitempty"`
}

func GetMetadata() (m *Metadata, err error) {
	resp, err := http.Get(metadataURL + "/conf?format=json")
	if err != nil {
		return nil, errors.Wrap(err, "error getting metadataURL")
	}
	defer resp.Body.Close()

	metadata := &Metadata{}
	err = json.NewDecoder(resp.Body).Decode(metadata)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding metadata")
	}
	return metadata, nil
}
