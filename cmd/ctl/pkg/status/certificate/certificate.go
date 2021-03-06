/*
Copyright 2020 The Jetstack cert-manager contributors.

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

package certificate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/reference"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmclient "github.com/jetstack/cert-manager/pkg/client/clientset/versioned"
	"github.com/jetstack/cert-manager/pkg/ctl"
	"github.com/jetstack/cert-manager/pkg/util/predicate"
)

var (
	long = templates.LongDesc(i18n.T(`
Get details about the current status of a cert-manager Certificate resource, including information on related resources like CertificateRequest.`))

	example = templates.Examples(i18n.T(`
# Query status of Certificate with name 'my-crt' in namespace 'my-namespace'
kubectl cert-manager status certificate my-crt --namespace my-namespace
`))
)

// Options is a struct to support status certificate command
type Options struct {
	CMClient   cmclient.Interface
	RESTConfig *restclient.Config
	// The Namespace that the Certificate to be queried about resides in.
	// This flag registration is handled by cmdutil.Factory
	Namespace string

	genericclioptions.IOStreams
}

// NewOptions returns initialized Options
func NewOptions(ioStreams genericclioptions.IOStreams) *Options {
	return &Options{
		IOStreams: ioStreams,
	}
}

// NewCmdStatusCert returns a cobra command for status certificate
func NewCmdStatusCert(ioStreams genericclioptions.IOStreams, factory cmdutil.Factory) *cobra.Command {
	o := NewOptions(ioStreams)
	cmd := &cobra.Command{
		Use:     "certificate",
		Short:   "Get details about the current status of a cert-manager Certificate resource",
		Long:    long,
		Example: example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Validate(args))
			cmdutil.CheckErr(o.Complete(factory))
			cmdutil.CheckErr(o.Run(args))
		},
	}
	return cmd
}

// Validate validates the provided options
func (o *Options) Validate(args []string) error {
	if len(args) < 1 {
		return errors.New("the name of the Certificate has to be provided as argument")
	}
	if len(args) > 1 {
		return errors.New("only one argument can be passed in: the name of the Certificate")
	}
	return nil
}

// Complete takes the factory and infers any remaining options.
func (o *Options) Complete(f cmdutil.Factory) error {
	var err error

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.RESTConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}

	o.CMClient, err = cmclient.NewForConfig(o.RESTConfig)
	if err != nil {
		return err
	}

	return nil
}

// Run executes status certificate command
func (o *Options) Run(args []string) error {
	ctx := context.TODO()
	crtName := args[0]

	clientSet, err := kubernetes.NewForConfig(o.RESTConfig)
	if err != nil {
		return err
	}

	crt, err := o.CMClient.CertmanagerV1alpha2().Certificates(o.Namespace).Get(ctx, crtName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error when getting Certificate resource: %v", err)
	}

	crtRef, err := reference.GetReference(ctl.Scheme, crt)
	if err != nil {
		return err
	}
	// Ignore error, since if there was an error, crtEvents would be nil and handled down the line in DescribeEvents
	crtEvents, _ := clientSet.CoreV1().Events(o.Namespace).Search(ctl.Scheme, crtRef)

	secret, secretErr := clientSet.CoreV1().Secrets(o.Namespace).Get(ctx, crt.Spec.SecretName, metav1.GetOptions{})
	if secretErr != nil {
		secretErr = fmt.Errorf("error when finding Secret %q: %w\n", crt.Spec.SecretName, secretErr)
	}

	// TODO: What about timing issues? When I query condition it's not ready yet, but then looking for cr it's finished and deleted
	// Try find the CertificateRequest that is owned by crt and has the correct revision
	req, reqErr := findMatchingCR(o.CMClient, ctx, crt)
	if reqErr != nil {
		reqErr = fmt.Errorf("error when finding CertificateRequest: %w\n", reqErr)
	}
	if req == nil {
		reqErr = errors.New("No CertificateRequest found for this Certificate\n")
	}

	var reqEvents *corev1.EventList
	if req != nil {
		reqRef, err := reference.GetReference(ctl.Scheme, req)
		if err != nil {
			return err
		}
		// Ignore error, since if there was an error, reqEvents would be nil and handled down the line in DescribeEvents
		reqEvents, _ = clientSet.CoreV1().Events(o.Namespace).Search(ctl.Scheme, reqRef)
	}

	// Build status of Certificate with data gathered
	status := newCertificateStatusFromCert(crt).
		withEvents(crtEvents).
		withSecret(secret, secretErr).
		withCR(req, reqEvents, reqErr)

	issuerKind := crt.Spec.IssuerRef.Kind
	if issuerKind == "" {
		issuerKind = "Issuer"
	}

	// Get info on Issuer/ClusterIssuer
	if crt.Spec.IssuerRef.Group != "cert-manager.io" && crt.Spec.IssuerRef.Group != "" {
		// TODO: Support Issuers/ClusterIssuers from other groups as well
		status = status.withIssuer(nil, fmt.Errorf("The %s %q is not of the group cert-manager.io, this command currently does not support third party issuers.\nTo get more information about %q, try 'kubectl describe'\n",
			issuerKind, crt.Spec.IssuerRef.Name, crt.Spec.IssuerRef.Name))
	} else if issuerKind == "Issuer" {
		issuer, issuerErr := o.CMClient.CertmanagerV1alpha2().Issuers(crt.Namespace).Get(ctx, crt.Spec.IssuerRef.Name, metav1.GetOptions{})
		if issuerErr != nil {
			issuerErr = fmt.Errorf("error when getting Issuer: %v\n", issuerErr)
		}
		status = status.withIssuer(issuer, issuerErr)
	} else {
		// ClusterIssuer
		clusterIssuer, issuerErr := o.CMClient.CertmanagerV1alpha2().ClusterIssuers().Get(ctx, crt.Spec.IssuerRef.Name, metav1.GetOptions{})
		if issuerErr != nil {
			issuerErr = fmt.Errorf("error when getting ClusterIssuer: %v\n", issuerErr)
		}
		status = status.withClusterIssuer(clusterIssuer, issuerErr)
	}

	fmt.Fprintf(o.Out, status.String())

	return nil
}

// formatStringSlice takes in a string slice and formats the contents of the slice
// into a single string where each element of the slice is prefixed with "- " and on a new line
func formatStringSlice(strings []string) string {
	result := ""
	for _, str := range strings {
		result += "- " + str + "\n"
	}
	return result
}

// formatTimeString returns the time as a string
// If nil, return "<none>"
func formatTimeString(t *metav1.Time) string {
	if t == nil {
		return "<none>"
	}
	return t.Time.Format(time.RFC3339)
}

// findMatchingCR tries to find a CertificateRequest that is owned by crt and has the correct revision annotated from reqs.
// If none found returns nil
// If one found returns the CR
// If multiple found or error occurs when listing CRs, returns error
func findMatchingCR(cmClient cmclient.Interface, ctx context.Context, crt *cmapi.Certificate) (*cmapi.CertificateRequest, error) {
	reqs, err := cmClient.CertmanagerV1alpha2().CertificateRequests(crt.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error when listing CertificateRequest resources: %w", err)
	}

	possibleMatches := []*cmapi.CertificateRequest{}

	// CertificateRequest revisions begin from 1.
	// If no revision is set on the Certificate then assume the revision on the CertificateRequest should be 1.
	// If revision is set on the Certificate then revision on the CertificateRequest should be crt.Status.Revision + 1.
	nextRevision := 1
	if crt.Status.Revision != nil {
		nextRevision = *crt.Status.Revision + 1
	}
	for _, req := range reqs.Items {
		if predicate.CertificateRequestRevision(nextRevision)(&req) &&
			predicate.ResourceOwnedBy(crt)(&req) {
			possibleMatches = append(possibleMatches, req.DeepCopy())
		}
	}

	if len(possibleMatches) < 1 {
		return nil, nil
	} else if len(possibleMatches) == 1 {
		return possibleMatches[0], nil
	} else {
		return nil, errors.New("found multiple certificate requests with expected revision and owner")
	}
}
