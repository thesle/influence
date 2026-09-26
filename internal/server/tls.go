// Influence — a self-hostable documentation platform.
// Copyright (C) 2026  Conrad Smith
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/influence/influence/internal/config"
)

// This file owns TLS listener construction and plaintext enforcement
// (Requirements 19.1, 19.2). It intentionally exposes small, side-effect-free
// construction helpers that the fail-fast startup sequence (task 2.2) calls in
// its fixed order: task 2.2 owns validation ordering and the actual bind
// timing, while this file owns *how* a listener and its TLS configuration are
// built and how plaintext requests are refused.

// TLSConfig builds the tls.Config used when SSL is enabled. The certificate and
// private key are loaded from the configured paths, which both validates that
// the pair is present, readable, and matched (a mismatched or unreadable pair
// surfaces here as an error the startup sequence reports per Requirement 19.4)
// and pins the negotiated protocol to TLS 1.2 or higher (Requirement 19.1).
//
// It returns an error when TLS is not enabled so callers never accidentally
// build a TLS server for a plaintext deployment.
func TLSConfig(cfg config.Config) (*tls.Config, error) {
	if !cfg.TLS {
		return nil, fmt.Errorf("server: TLS is not enabled")
	}
	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		return nil, fmt.Errorf("server: TLS enabled but certificate or key path is empty")
	}

	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		// Covers missing, unreadable, or mismatched cert/key (Requirement
		// 19.4). The startup sequence turns this into a fail-to-start.
		return nil, fmt.Errorf("server: loading TLS certificate/key: %w", err)
	}

	return &tls.Config{
		// Serve client traffic only over TLS 1.2 or higher (Requirement 19.1).
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// BuildListener binds a network listener for the resolved port. When TLS is
// enabled it returns a TLS listener whose configuration pins TLS 1.2+
// (Requirement 19.1); when TLS is disabled it returns a plaintext TCP listener.
//
// The startup sequence (task 2.2) is responsible for ordering this after all
// prior fail-fast validations and for reporting a port-already-in-use error;
// BuildListener simply performs the bind and surfaces any error to the caller.
func BuildListener(cfg config.Config) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", cfg.Port)

	if !cfg.TLS {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("server: binding %s: %w", addr, err)
		}
		return ln, nil
	}

	tlsCfg, err := TLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("server: binding TLS listener on %s: %w", addr, err)
	}
	// Binding only a TLS listener means the server never accepts plaintext on
	// the serving port when SSL is configured (Requirement 19.2).
	return ln, nil
}

// PlaintextGuard wraps next so that, when SSL is configured, any request that
// arrives without a completed TLS handshake is refused rather than served over
// plaintext (Requirement 19.2). Because BuildListener binds a TLS-only listener
// when SSL is enabled, plaintext bytes are normally rejected at the transport
// layer; this guard is the defense-in-depth HTTP-level check that also covers a
// misconfigured reverse proxy forwarding plaintext, and it belongs at the head
// of the middleware chain (design § Request / Authorization Middleware).
//
// When SSL is not configured, the guard is a pass-through: plaintext is the
// intended transport and requests flow to next unchanged.
func PlaintextGuard(tlsEnabled bool, next http.Handler) http.Handler {
	if !tlsEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			http.Error(w, "TLS required: plaintext requests are refused", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
