package tpmdevid

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/hcl"
	nodeattestorv1 "github.com/spiffe/spire-plugin-sdk/proto/spire/plugin/agent/nodeattestor/v1"
	configv1 "github.com/spiffe/spire-plugin-sdk/proto/spire/service/common/config/v1"
	"github.com/spiffe/spire/pkg/agent/plugin/nodeattestor/tpmdevid/tpmutil"
	"github.com/spiffe/spire/pkg/common/catalog"
	common_devid "github.com/spiffe/spire/pkg/common/plugin/tpmdevid"
	"github.com/spiffe/spire/pkg/common/pluginconf"
	"github.com/spiffe/spire/pkg/common/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const BaseTPMDir = "/dev"

// Functions defined here are overridden in test files to facilitate unit testing
var (
	AutoDetectTPMPath func(string) (string, error)                           = tpmutil.AutoDetectTPMPath
	NewSession        func(*tpmutil.SessionConfig) (*tpmutil.Session, error) = tpmutil.NewSession
)

func BuiltIn() catalog.BuiltIn {
	return builtin(New())
}

func builtin(p *Plugin) catalog.BuiltIn {
	return catalog.MakeBuiltIn(common_devid.PluginName,
		nodeattestorv1.NodeAttestorPluginServer(p),
		configv1.ConfigServiceServer(p))
}

type Config struct {
	DevIDPrivPath string `hcl:"devid_priv_path"`
	DevIDPubPath  string `hcl:"devid_pub_path"`
	DevIDCertPath string `hcl:"devid_cert_path"`

	DevIDKeyPassword             string `hcl:"devid_password"`
	OwnerHierarchyPassword       string `hcl:"owner_hierarchy_password"`
	EndorsementHierarchyPassword string `hcl:"endorsement_hierarchy_password"`

	DevicePath string `hcl:"tpm_device_path"`
	Autodetect bool
}

func buildConfig(coreConfig catalog.CoreConfig, hclText string, status *pluginconf.Status) *Config {
	newConfig := new(Config)
	if err := hcl.Decode(newConfig, hclText); err != nil {
		status.ReportErrorf("unable to decode configuration: %v", err)
		return nil
	}

	if newConfig.DevIDCertPath == "" {
		status.ReportError("invalid configuration: devid_cert_path is required")
	}

	if newConfig.DevIDPrivPath == "" {
		status.ReportError("invalid configuration: devid_priv_path is required")
	}

	if newConfig.DevIDPubPath == "" {
		status.ReportError("invalid configuration: devid_pub_path is required")
	}

	if newConfig.DevicePath != "" && runtime.GOOS == "windows" {
		status.ReportError("device path is not allowed on windows")
	}

	if newConfig.DevicePath == "" && runtime.GOOS != "windows" {
		newConfig.Autodetect = true
	}

	return newConfig
}

type config struct {
	devicePath string
	devIDCert  [][]byte
	devIDPub   []byte
	devIDPriv  []byte
	passwords  tpmutil.TPMPasswords
}

type Plugin struct {
	nodeattestorv1.UnsafeNodeAttestorServer
	configv1.UnsafeConfigServer
	log hclog.Logger

	m sync.Mutex
	c *config
}

func New() *Plugin {
	return &Plugin{
		c: &config{},
	}
}

func (p *Plugin) AidAttestation(stream nodeattestorv1.NodeAttestor_AidAttestationServer) error {
	conf := p.getConfig()
	if conf == nil {
		return status.Error(codes.FailedPrecondition, "not configured")
	}

	// Open TPM connection and load DevID keys
	tpm, err := NewSession(&tpmutil.SessionConfig{
		DevicePath: conf.devicePath,
		DevIDPriv:  conf.devIDPriv,
		DevIDPub:   conf.devIDPub,
		Passwords:  conf.passwords,
		Log:        p.log,
	})
	if err != nil {
		return status.Errorf(codes.Internal, "unable to start a new TPM session: %v", err)
	}
	defer tpm.Close()

	// Get endorsement certificate from TPM NV index
	ekCert, err := tpm.GetEKCert()
	if err != nil {
		return status.Errorf(codes.Internal, "unable to get endorsement certificate: %v", err)
	}

	// Get regenerated endorsement public key
	ekPub, err := tpm.GetEKPublic()
	if err != nil {
		return status.Errorf(codes.Internal, "unable to get endorsement public key: %v", err)
	}

	// Certify DevID is in the same TPM than AK
	id, sig, err := tpm.CertifyDevIDKey()
	if err != nil {
		return status.Errorf(codes.Internal, "unable to certify DevID key: %v", err)
	}

	// Marshal attestation data
	marshaledAttData, err := json.Marshal(common_devid.AttestationRequest{
		DevIDCert: conf.devIDCert,
		DevIDPub:  conf.devIDPub,

		EKCert: ekCert,
		EKPub:  ekPub,

		AKPub: tpm.GetAKPublic(),

		CertifiedDevID:         id,
		CertificationSignature: sig,
	})
	if err != nil {
		return status.Errorf(codes.Internal, "unable to marshal attestation data: %v", err)
	}

	// Send attestation request
	err = stream.Send(&nodeattestorv1.PayloadOrChallengeResponse{
		Data: &nodeattestorv1.PayloadOrChallengeResponse_Payload{
			Payload: marshaledAttData,
		},
	})
	if err != nil {
		st := status.Convert(err)
		return status.Errorf(st.Code(), "unable to send attestation data: %s", st.Message())
	}

	// Receive challenges
	marshalledChallenges, err := stream.Recv()
	if err != nil {
		st := status.Convert(err)
		return status.Errorf(st.Code(), "unable to receive challenges: %s", st.Message())
	}

	challenges := &common_devid.ChallengeRequest{}
	if err = json.Unmarshal(marshalledChallenges.Challenge, challenges); err != nil {
		return status.Errorf(codes.InvalidArgument, "unable to unmarshall challenges: %v", err)
	}

	// Solve DevID challenge (verify the possession of the DevID private key)
	devIDChallengeResp, err := tpm.SolveDevIDChallenge(challenges.DevID)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to solve proof of possession challenge: %v", err)
	}

	// Solve Credential Activation challenge
	var credActChallengeResp []byte
	if challenges.CredActivation == nil {
		return status.Error(codes.Internal, "received empty credential activation challenge from server")
	}

	credActChallengeResp, err = tpm.SolveCredActivationChallenge(
		challenges.CredActivation.Credential,
		challenges.CredActivation.Secret)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to solve proof of residency challenge: %v", err)
	}

	// Marshal challenges responses
	marshalledChallengeResp, err := json.Marshal(common_devid.ChallengeResponse{
		DevID:          devIDChallengeResp,
		CredActivation: credActChallengeResp,
	})
	if err != nil {
		return status.Errorf(codes.Internal, "unable to marshal challenge response: %v", err)
	}

	// Send challenge response back to the server
	err = stream.Send(&nodeattestorv1.PayloadOrChallengeResponse{
		Data: &nodeattestorv1.PayloadOrChallengeResponse_ChallengeResponse{
			ChallengeResponse: marshalledChallengeResp,
		},
	})
	if err != nil {
		st := status.Convert(err)
		return status.Errorf(st.Code(), "unable to send challenge response: %s", st.Message())
	}

	return nil
}

func (p *Plugin) Configure(_ context.Context, req *configv1.ConfigureRequest) (*configv1.ConfigureResponse, error) {
	newConfig, _, err := pluginconf.Build(req, buildConfig)
	if err != nil {
		return nil, err
	}

	if newConfig.Autodetect {
		tpmPath, err := AutoDetectTPMPath(BaseTPMDir)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "tpm autodetection failed: %v", err)
		}
		newConfig.DevicePath = tpmPath
	}

	p.m.Lock()
	defer p.m.Unlock()

	p.c.devicePath = newConfig.DevicePath

	err = p.loadDevIDFiles(newConfig)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to load DevID files: %v", err)
	}

	p.c.passwords.DevIDKey = newConfig.DevIDKeyPassword
	p.c.passwords.OwnerHierarchy = newConfig.OwnerHierarchyPassword
	p.c.passwords.EndorsementHierarchy = newConfig.EndorsementHierarchyPassword

	return &configv1.ConfigureResponse{}, nil
}

func (p *Plugin) Validate(_ context.Context, req *configv1.ValidateRequest) (*configv1.ValidateResponse, error) {
	_, notes, err := pluginconf.Build(req, buildConfig)

	return &configv1.ValidateResponse{
		Valid: err == nil,
		Notes: notes,
	}, nil
}

func (p *Plugin) SetLogger(log hclog.Logger) {
	p.log = log
}

func (p *Plugin) getConfig() *config {
	p.m.Lock()
	defer p.m.Unlock()
	return p.c
}

func (p *Plugin) loadDevIDFiles(c *Config) error {
	certs, err := util.LoadCertificates(c.DevIDCertPath)
	if err != nil {
		return fmt.Errorf("cannot load certificate(s): %w", err)
	}

	for _, cert := range certs {
		p.c.devIDCert = append(p.c.devIDCert, cert.Raw)
	}

	p.c.devIDPriv, err = os.ReadFile(c.DevIDPrivPath)
	if err != nil {
		return fmt.Errorf("cannot load private key: %w", err)
	}

	p.c.devIDPub, err = os.ReadFile(c.DevIDPubPath)
	if err != nil {
		return fmt.Errorf("cannot load public key: %w", err)
	}

	return nil
}
