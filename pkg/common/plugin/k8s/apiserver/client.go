package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"

	authv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client is a client for querying k8s API server
type Client interface {
	// GetNode returns the node object for the given node name
	GetNode(ctx context.Context, nodeName string) (*v1.Node, error)

	// GetPod returns the pod object for the given pod name and namespace
	GetPod(ctx context.Context, namespace, podName string) (*v1.Pod, error)

	// ValidateToken queries k8s token review API and returns information about the given token
	ValidateToken(ctx context.Context, token string, audiences []string) (*authv1.TokenReviewStatus, error)
}

type client struct {
	kubeConfigFilePath string

	// loadClientHook is used to inject a fake loadClient on tests
	loadClientHook func(string) (kubernetes.Interface, error)
}

// New creates a new Client.
// There are two cases:
// - If a kubeConfigFilePath is provided, config is taken from that file -> use for clients running out of a k8s cluster
// - If not (empty kubeConfigFilePath), InClusterConfig is used          -> use for clients running in a k8s cluster
func New(kubeConfigFilePath string) Client {
	return &client{
		kubeConfigFilePath: kubeConfigFilePath,
		loadClientHook:     loadClient,
	}
}

func (c *client) GetPod(ctx context.Context, namespace, podName string) (*v1.Pod, error) {
	// Validate inputs
	if namespace == "" {
		return nil, errors.New("empty namespace")
	}
	if podName == "" {
		return nil, errors.New("empty pod name")
	}

	// Reload config
	clientset, err := c.loadClientHook(c.kubeConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to get clientset: %w", err)
	}

	// Get pod
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to query pods API: %w", err)
	}

	return pod, nil
}

func (c *client) GetNode(ctx context.Context, nodeName string) (*v1.Node, error) {
	// Validate inputs
	if nodeName == "" {
		return nil, errors.New("empty node name")
	}

	// Reload config
	clientset, err := c.loadClientHook(c.kubeConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to get clientset: %w", err)
	}

	// Get node
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to query nodes API: %w", err)
	}

	return node, nil
}

func (c *client) ValidateToken(ctx context.Context, token string, audiences []string) (*authv1.TokenReviewStatus, error) {
	// Reload config
	clientset, err := c.loadClientHook(c.kubeConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to get clientset: %w", err)
	}

	// Create token review request
	req := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: audiences,
		},
	}

	// Do request
	resp, err := clientset.AuthenticationV1().TokenReviews().Create(ctx, req, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to query token review API: %w", err)
	}

	// Evaluate token review response (review server will populate TokenReview.Status field)
	if resp.Status.Error != "" {
		return nil, fmt.Errorf("token review API response contains an error: %v", resp.Status.Error)
	}

	// Ensure the audiences returned in the status are compatible with those requested
	// in the TokenReviewSpec (if any). This is to ensure the validator is
	// audience aware.
	// See the documentation on the Status Audiences field.
	if resp.Status.Authenticated && len(audiences) > 0 {
		atLeastOnePresent := false
		for _, audience := range audiences {
			if slices.Contains(resp.Status.Audiences, audience) {
				atLeastOnePresent = true
				break
			}
		}
		if !atLeastOnePresent {
			return nil, fmt.Errorf("token review API did not validate audience: wanted one of %q but got %q", audiences, resp.Status.Audiences)
		}
	}

	return &resp.Status, nil
}

func loadClient(kubeConfigFilePath string) (kubernetes.Interface, error) {
	var config *rest.Config
	var err error

	if kubeConfigFilePath == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfigFilePath)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to create client config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create clientset for the given config: %w", err)
	}

	return clientset, nil
}
