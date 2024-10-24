// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"github.com/dgrijalva/jwt-go"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/upbound/universal-crossplane/internal/clients/upbound"
	"github.com/upbound/universal-crossplane/internal/upboundagent"
	"github.com/upbound/universal-crossplane/internal/version"
)

const (
	prefixPlatformTokenSubject   = "controlPlane|"
	controlPlaneTokenCheckPeriod = time.Second * 3
)

const (
	errMalformedCPToken          = "malformed control plane token"
	errCPTokenNoSubjectKey       = "failed to get value for key \"sub\""
	errCPTokenSubjectIsNotString = "failed to parse value for key \"sub\" as a string"
	errCPIDInTokenNotValidUUID   = "control plane id in token is not a valid UUID: %s"
	errFailedToGetKubeSystemNS   = "failed to get kube-system namespace"
	errKubeSystemUIDEmpty        = "metadata.uid of kube-system namespace is empty"
)

// AgentCmd represents the "upbound-agent" command
type AgentCmd struct {
	PodName               string `help:"Name of the agent pod."`
	ServerPort            string `default:"6443" help:"Port to serve agent service."`
	TLSCertFile           string `help:"File containing the default x509 Certificate for HTTPS."`
	TLSKeyFile            string `help:"File containing the default x509 private key matching provided cert"`
	XgqlCABundleFile      string `help:"CA bundle file for xgql server"`
	NATSEndpoint          string `help:"Endpoint for nats"`
	UpboundAPIEndpoint    string `help:"Endpoint for Upbound API"`
	ControlPlaneTokenPath string `help:"File path of the platform token to access Upbound Cloud connect endpoint"`
}

var cli struct {
	Debug bool `help:"Enable debug mode"`

	Agent AgentCmd `cmd:"" help:"Runs Upbound Agent"`
}

func main() { // nolint:gocyclo
	ctx := kong.Parse(&cli)
	zl := zap.New(zap.UseDevMode(cli.Debug))
	log := logging.NewLogrLogger(zl.WithName("upbound-agent"))
	a := cli.Agent

	token, err := waitForControlPlaneToken(a.ControlPlaneTokenPath, controlPlaneTokenCheckPeriod, log)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to wait for control plane token"))
	}

	cpID, err := readCPIDFromToken(token)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to read control plane id from token"))
	}

	upClient := upbound.NewClient(a.UpboundAPIEndpoint, log, cli.Debug)
	pubCerts, err := upClient.GetGatewayCerts(token)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to fetch public certs"))
	}
	pem, err := base64.StdEncoding.DecodeString(pubCerts.JWTPublicKey)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to base64 decode provided jwt public key"))
	}

	pk, err := jwt.ParseRSAPublicKeyFromPEM(pem)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to parse public key"))
	}

	var xgqlCertPool *x509.CertPool
	if a.XgqlCABundleFile != "" {
		b, err := os.ReadFile(filepath.Clean(a.XgqlCABundleFile))
		if err != nil {
			ctx.FatalIfErrorf(errors.Wrap(err, "failed to read xgql ca bundle file"))
		}
		xgqlCertPool, err = generateTrustedCertPool(b)
		if err != nil {
			ctx.FatalIfErrorf(errors.Wrap(err, "failed to generate xgql ca cert pool"))
		}
	}

	tgConfig := &upboundagent.Config{
		DebugMode:         cli.Debug,
		ControlPlaneID:    cpID,
		TokenRSAPublicKey: pk,
		XGQLCACertPool:    xgqlCertPool,
		NATS: &upboundagent.NATSClientConfig{
			Name:              a.PodName,
			Endpoint:          a.NATSEndpoint,
			JWTEndpoint:       a.UpboundAPIEndpoint,
			ControlPlaneToken: token,
			CABundle:          pubCerts.NATSCA,
		},
	}

	restConfig, err := config.GetConfig()
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to get rest config"))
	}
	kube, err := client.New(restConfig, client.Options{})
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to initialize kubernetes client"))
	}
	kubeClusterID, err := readKubeClusterID(kube)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to read kube cluster ID"))
	}

	pxy, err := upboundagent.NewProxy(tgConfig, restConfig, upClient, log, kubeClusterID)
	if err != nil {
		ctx.FatalIfErrorf(errors.Wrap(err, "failed to create new agent proxy"))
	}

	log.Info("Starting Upbound Agent ", "version", version.Version,
		"control-plane-id", cpID,
		"debug", cli.Debug,
		"pod-name", a.PodName,
		"server-port", a.ServerPort,
		"tls-cert-file", a.TLSCertFile,
		"tls-private-key-file", a.TLSKeyFile,
		"xgql-ca-bundle-file", a.XgqlCABundleFile,
		"nats-endpoint", a.NATSEndpoint,
		"upbound-api-endpoint", a.UpboundAPIEndpoint)

	addr := fmt.Sprintf(":%s", a.ServerPort)
	ctx.FatalIfErrorf(errors.Wrap(pxy.Run(addr, a.TLSCertFile, a.TLSKeyFile), "cannot run upbound agent proxy"))
}

func waitForControlPlaneToken(path string, d time.Duration, log logging.Logger) (string, error) {
	ticker := time.NewTicker(d)
	log.Info("waiting for control plane token to be mounted", "path", path, "check-period", d.String())
	defer ticker.Stop()
	for {
		// We should wait until file exists and has content.
		f, err := os.ReadFile(filepath.Clean(path))
		if resource.Ignore(os.IsNotExist, err) != nil {
			return "", errors.Wrapf(err, "cannot read control plane token file")
		}
		if len(f) != 0 {
			log.Info("control plane token has been read")
			return string(f), nil
		}
		log.Debug("control plane token file is empty")
		// TODO(muvaf): We can probably replace this with an implementation
		// that uses time.Ticker but I couldn't figure out to have a simple
		// implementation with no timeout and single select case without wasting
		// the initial period.
		time.Sleep(d)
	}
}

func generateTrustedCertPool(b []byte) (*x509.CertPool, error) {
	rootCAs := x509.NewCertPool()

	if ok := rootCAs.AppendCertsFromPEM(b); !ok {
		return nil, errors.New("no certs appended, ca cert should have been appended")
	}

	return rootCAs, nil
}

func readCPIDFromToken(t string) (string, error) {
	// Read control-plane id from the token
	token, err := jwt.Parse(t, nil)
	if err.(*jwt.ValidationError).Errors == jwt.ValidationErrorMalformed {
		return "", errors.Wrap(err, errMalformedCPToken)
	}

	cl := token.Claims.(jwt.MapClaims)
	v, ok := cl["sub"]
	if !ok {
		return "", errors.New(errCPTokenNoSubjectKey)
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New(errCPTokenSubjectIsNotString)
	}

	envID := strings.TrimPrefix(s, prefixPlatformTokenSubject)
	_, err = uuid.Parse(envID)
	return envID, errors.Wrapf(err, errCPIDInTokenNotValidUUID, envID)
}

func readKubeClusterID(kube client.Client) (string, error) {
	ns := &corev1.Namespace{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: "kube-system"}, ns); err != nil {
		return "", errors.Wrap(err, errFailedToGetKubeSystemNS)
	}
	if ns.GetUID() == "" {
		return "", errors.New(errKubeSystemUIDEmpty)
	}
	return string(ns.GetUID()), nil
}
