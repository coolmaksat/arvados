// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package boot

import (
	"context"
	"io/ioutil"
	"path/filepath"
)

// Create a root CA key and use it to make a new server
// certificate+key pair.
//
// In future we'll make one root CA key per host instead of one per
// cluster, so it only needs to be imported to a browser once for
// ongoing dev/test usage.
type createCertificates struct{}

func (createCertificates) String() string {
	return "certificates"
}

func (createCertificates) Run(ctx context.Context, fail func(error), super *Supervisor) error {
	// Generate root key
	err := super.RunProgram(ctx, super.tempdir, nil, nil, "openssl", "genrsa", "-out", "rootCA.key", "4096")
	if err != nil {
		return err
	}
	// Generate a self-signed root certificate
	err = super.RunProgram(ctx, super.tempdir, nil, nil, "openssl", "req", "-x509", "-new", "-nodes", "-key", "rootCA.key", "-sha256", "-days", "3650", "-out", "rootCA.crt", "-subj", "/C=US/ST=MA/O=Example Org/CN=localhost")
	if err != nil {
		return err
	}
	// Generate server key
	err = super.RunProgram(ctx, super.tempdir, nil, nil, "openssl", "genrsa", "-out", "server.key", "2048")
	if err != nil {
		return err
	}
	// Build config file for signing request
	defaultconf, err := ioutil.ReadFile("/etc/ssl/openssl.cnf")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(super.tempdir, "server.cfg"), append(defaultconf, []byte(`
[SAN]
subjectAltName=DNS:localhost,DNS:localhost.localdomain
`)...), 0644)
	if err != nil {
		return err
	}
	// Generate signing request
	err = super.RunProgram(ctx, super.tempdir, nil, nil, "openssl", "req", "-new", "-sha256", "-key", "server.key", "-subj", "/C=US/ST=MA/O=Example Org/CN=localhost", "-reqexts", "SAN", "-config", "server.cfg", "-out", "server.csr")
	if err != nil {
		return err
	}
	// Sign certificate
	err = super.RunProgram(ctx, super.tempdir, nil, nil, "openssl", "x509", "-req", "-in", "server.csr", "-CA", "rootCA.crt", "-CAkey", "rootCA.key", "-CAcreateserial", "-out", "server.crt", "-days", "3650", "-sha256")
	if err != nil {
		return err
	}
	return nil
}
