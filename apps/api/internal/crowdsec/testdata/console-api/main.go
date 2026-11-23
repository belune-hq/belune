// An offline CAPI protocol fixture for the real cscli enrollment drill. It runs
// on loopback inside a network=none CrowdSec container; no request can escape.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"sync"
	"time"
)

func main() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "api.crowdsec.net"}, DNSNames: []string{"api.crowdsec.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile("/tmp/belune-console-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		panic(err)
	}
	if err = os.WriteFile("/tmp/belune-console-key.pem", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		panic(err)
	}
	var mu sync.Mutex
	rateLimited := ""
	counts := struct {
		Logins      int    `json:"logins"`
		Enrollments int    `json:"enrollments"`
		Name        string `json:"name"`
		Auto        bool   `json:"auto"`
		KeyPresent  bool   `json:"key_present"`
		Overwrite   bool   `json:"overwrite"`
	}{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/watchers/login", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts.Logins++
		limited := rateLimited
		mu.Unlock()
		if limited != "" {
			w.Header().Set("Content-Type", "application/json")
			if limited == "html" {
				w.Header().Set("Content-Type", "text/html")
			}
			if limited == "text" {
				w.Header().Set("Content-Type", "text/plain")
			}
			w.WriteHeader(http.StatusForbidden)
			switch limited {
			case "empty":
			case "html":
				_, _ = w.Write([]byte("<html>403</html>"))
			case "text":
				_, _ = w.Write([]byte("rate limit reached"))
			case "json_missing":
				_, _ = w.Write([]byte(`{"detail":"rate limit"}`))
			default:
				_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
			}
			return
		}
		payload, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "offline-watcher"})
		token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expire": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("/v3/watchers/enroll", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name      string `json:"name"`
			Auto      bool   `json:"autoenroll"`
			Key       string `json:"attachment_key"`
			Overwrite bool   `json:"overwrite"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		counts.Enrollments++
		counts.Name = request.Name
		counts.Auto = request.Auto
		counts.KeyPresent = request.Key != ""
		counts.Overwrite = request.Overwrite
		n := counts.Enrollments
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n > 1 && !request.Overwrite {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "https://app.crowdsec.net/enroll/offline-fixture", "expire_at": time.Now().Add(15 * time.Minute).UnixMilli()})
	})
	go func() {
		if err := http.ListenAndServeTLS("127.0.0.1:443", "/tmp/belune-console-ca.pem", "/tmp/belune-console-key.pem", mux); err != nil {
			panic(err)
		}
	}()
	status := http.NewServeMux()
	status.HandleFunc("/rate-limit", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rateLimited = r.URL.Query().Get("shape")
		if rateLimited == "" {
			rateLimited = "message"
		}
		mu.Unlock()
		w.WriteHeader(204)
	})
	status.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(counts)
	})
	if err := http.ListenAndServe("127.0.0.1:8088", status); err != nil {
		panic(err)
	}
}
