package provider

import (
	"fmt"
	"context"
	"crypto/tls"
	"path/filepath"
	"sync"

	"github.com/lyft/goruntime/loader"
	gostats "github.com/lyft/gostats"
	logger "github.com/goatapp/ratelimit/src/log"

	"github.com/goatapp/ratelimit/src/settings"
)

// CertProvider will watch certDirectory for changes via goruntime/loader and reload the cert and key files
type CertProvider struct {
	settings           settings.Settings
	runtime            loader.IFace
	runtimeUpdateEvent chan int
	rootStore          gostats.Store
	certLock           sync.RWMutex
	cert               *tls.Certificate
	certDirectory      string
	certFile           string
	keyFile            string
}

// GetCertificateFunc returns a function compatible with tls.Config.GetCertificate, fetching the current certificate
func (p *CertProvider) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		p.certLock.RLock()
		defer p.certLock.RUnlock()
		return p.cert, nil
	}
}

func (p *CertProvider) watch() {
	p.runtime.AddUpdateCallback(p.runtimeUpdateEvent)

	go func() {
		for {
			logger.Debug(context.Background(), "CertProvider: waiting for runtime update")
			<-p.runtimeUpdateEvent
			logger.Debug(context.Background(), "CertProvider: got runtime update and reloading config")
			p.reloadCert()
		}
	}()
}

// reloadCert loads the cert and key files and updates the tls.Certificate in memory
func (p *CertProvider) reloadCert() {
	tlsKeyPair, err := tls.LoadX509KeyPair(p.certFile, p.keyFile)
	if err != nil {
		logger.Error(context.Background(), fmt.Sprintf("CertProvider failed to load TLS key pair (%s, %s): %v", p.certFile, p.keyFile, err))
		// panic in case there is no cert already loaded as this would mean starting up without TLS
		if p.cert == nil {
			logger.Fatal(context.Background(), "CertProvider failed to load any certificate, exiting.")
		}
		return // keep the old cert if we have one
	}
	p.certLock.Lock()
	defer p.certLock.Unlock()
	p.cert = &tlsKeyPair
	logger.Info(context.Background(), fmt.Sprintf("CertProvider reloaded cert from (%s, %s)", p.certFile, p.keyFile))
}

// setupRuntime sets up the goruntime loader to watch the certDirectory
// Will panic if it fails to set up the loader
func (p *CertProvider) setupRuntime() {
	var err error

	// runtimePath is the parent folder of certPath
	runtimePath := filepath.Dir(p.certDirectory)
	// runtimeSubdirectory is the name of the folder to watch, containing the certs
	runtimeSubdirectory := filepath.Base(p.certDirectory)

	p.runtime, err = loader.New2(
		runtimePath,
		runtimeSubdirectory,
		p.rootStore.ScopeWithTags("certs", p.settings.ExtraTags),
		&loader.DirectoryRefresher{},
		loader.IgnoreDotFiles)
	if err != nil {
		logger.Fatal(context.Background(), fmt.Sprintf("Failed to set up goruntime loader: %v", err))
	}
}

// NewCertProvider creates a new CertProvider
// Will panic if it fails to set up gruntime or fails to load the initial certificate
func NewCertProvider(settings settings.Settings, rootStore gostats.Store, certFile, keyFile string) *CertProvider {
	certDirectory := filepath.Dir(certFile)
	if certDirectory != filepath.Dir(keyFile) {
		logger.Fatal(context.Background(), "certFile and keyFile must be in the same directory")
	}
	p := &CertProvider{
		settings:           settings,
		runtimeUpdateEvent: make(chan int),
		rootStore:          rootStore,
		certDirectory:      certDirectory,
		certFile:           certFile,
		keyFile:            keyFile,
	}
	p.setupRuntime()
	// Initially load the certificate (or panic)
	p.reloadCert()
	go p.watch()
	return p
}
