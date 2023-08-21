//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmetav1 "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	opv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/cert-manager-operator/api/operator/v1alpha1"
	certmanoperatorclient "github.com/openshift/cert-manager-operator/pkg/operator/clientset/versioned"
	"github.com/openshift/cert-manager-operator/test/library"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

var defaultCertManagerState = v1alpha1.CertManager{
	ObjectMeta: v1.ObjectMeta{
		Name: "cluster",
	},
	Spec: v1alpha1.CertManagerSpec{
		OperatorSpec: opv1.OperatorSpec{
			ManagementState: opv1.Managed,
		},
	},
}

var subscriptionSchema = schema.GroupVersionResource{
	Group:    "operators.coreos.com",
	Version:  "v1alpha1",
	Resource: "subscriptions",
}

// verifyOperatorStatusCondition polls every 1 second to check if the status of each of the controllers
// match with the expected conditions. It returns an error if a timeout (5 mins) occurs or an error was
// encountered which polling the status. For each controller a the polling happens in separate go-routines.
func verifyOperatorStatusCondition(client *certmanoperatorclient.Clientset, controllerNames []string, expectedConditionMap map[string]opv1.ConditionStatus) error {

	var wg sync.WaitGroup
	errs := make([]error, len(controllerNames))
	for index := range controllerNames {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			err := wait.PollImmediate(time.Second*1, time.Minute*5, func() (done bool, err error) {
				operator, err := client.OperatorV1alpha1().CertManagers().Get(context.TODO(), "cluster", v1.GetOptions{})
				if err != nil {
					if apierrors.IsNotFound(err) {
						return false, nil
					}
					return false, err
				}

				if operator.DeletionTimestamp != nil {
					return false, nil
				}

				for _, cond := range operator.Status.Conditions {
					if status, exists := expectedConditionMap[strings.TrimPrefix(cond.Type, controllerNames[index])]; exists {
						if cond.Status != status {
							return false, nil
						}
					}
				}

				return true, nil
			})
			errs[index] = err
		}(index)
	}
	wg.Wait()

	return errors.NewAggregate(errs)
}

// removeOverrides removes all the overrides from all the cert-manager operands. The update process is retried if
// a conflict error is encountered.
func removeOverrides(client *certmanoperatorclient.Clientset) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		operator, err := client.OperatorV1alpha1().CertManagers().Get(context.TODO(), "cluster", v1.GetOptions{})
		if err != nil {
			return err
		}

		updatedOperator := operator.DeepCopy()

		hasOverride := false
		if updatedOperator.Spec.ControllerConfig != nil {
			updatedOperator.Spec.ControllerConfig = nil
			hasOverride = true
		}
		if updatedOperator.Spec.WebhookConfig != nil {
			updatedOperator.Spec.WebhookConfig = nil
			hasOverride = true
		}
		if updatedOperator.Spec.CAInjectorConfig != nil {
			updatedOperator.Spec.CAInjectorConfig = nil
			hasOverride = true
		}

		if !hasOverride {
			return nil
		}

		_, err = client.OperatorV1alpha1().CertManagers().Update(context.TODO(), updatedOperator, v1.UpdateOptions{})
		return err
	})

}

// addOverrideArgs adds the override args to specific the cert-manager operand. The update process is retried if
// a conflict error is encountered.
func addOverrideArgs(client *certmanoperatorclient.Clientset, deploymentName string, args []string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		operator, err := client.OperatorV1alpha1().CertManagers().Get(context.TODO(), "cluster", v1.GetOptions{})
		if err != nil {
			return err
		}

		updatedOperator := operator.DeepCopy()

		switch deploymentName {
		case certmanagerControllerDeployment:
			updatedOperator.Spec.ControllerConfig = &v1alpha1.DeploymentConfig{
				OverrideArgs: args,
			}
		case certmanagerWebhookDeployment:
			updatedOperator.Spec.WebhookConfig = &v1alpha1.DeploymentConfig{
				OverrideArgs: args,
			}
		case certmanagerCAinjectorDeployment:
			updatedOperator.Spec.CAInjectorConfig = &v1alpha1.DeploymentConfig{
				OverrideArgs: args,
			}
		default:
			return fmt.Errorf("unsupported deployment name: %s", deploymentName)
		}

		_, err = client.OperatorV1alpha1().CertManagers().Update(context.TODO(), updatedOperator, v1.UpdateOptions{})
		return err
	})
}

// verifyDeploymentArgs polls every 1 second to check if the deployment args list is updated to contain the
// passed args. It returns an error if a timeout (5 mins) occurs or an error was encountered while polling
// the deployment args list.
func verifyDeploymentArgs(k8sclient *kubernetes.Clientset, deploymentName string, args []string, added bool) error {

	return wait.PollImmediate(time.Second*1, time.Minute*5, func() (done bool, err error) {
		controllerDeployment, err := k8sclient.AppsV1().Deployments(operandNamespace).Get(context.TODO(), deploymentName, v1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		if len(controllerDeployment.Spec.Template.Spec.Containers) == 0 {
			return false, fmt.Errorf("%s deployment spec does not have container information", deploymentName)
		}

		containerArgsSet := sets.New[string](controllerDeployment.Spec.Template.Spec.Containers[0].Args...)

		if added {
			if !containerArgsSet.HasAll(args...) {
				return false, nil
			}
		} else {
			if containerArgsSet.HasAll(args...) {
				return false, nil
			}
		}

		return true, nil
	})
}

// getCertManagerOperatorSubscription returns the name of the first subscription object by listing
// them in the cert-manager-operator namespace using a k8s dynamic client
func getCertManagerOperatorSubscription(ctx context.Context, loader library.DynamicResourceLoader) (string, error) {
	subscriptionClient := loader.DynamicClient.Resource(subscriptionSchema).Namespace("cert-manager-operator")

	subs, err := subscriptionClient.List(ctx, v1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(subs.Items) == 0 {
		return "", fmt.Errorf("no subscription object found in operator namespace")
	}

	subName, ok := subs.Items[0].Object["metadata"].(map[string]interface{})["name"].(string)
	if !ok {
		return "", fmt.Errorf("could not parse metadata.name from the first subscription object found")
	}
	return subName, nil
}

// patchSubscriptionWithCloudCredential uses the k8s dynamic client to patche the only Subscription object
// in the cert-manager-operator namespace to inject CLOUD_CREDENTIALS_SECRET_NAME="aws-creds" env
// into its spec.config.env
func patchSubscriptionWithCloudCredential(ctx context.Context, loader library.DynamicResourceLoader) error {
	subName, err := getCertManagerOperatorSubscription(ctx, loader)
	if err != nil {
		return err
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"config": map[string]interface{}{
				"env": []interface{}{
					map[string]interface{}{
						"name":  "CLOUD_CREDENTIALS_SECRET_NAME",
						"value": "aws-creds",
					},
				},
			},
		},
	}
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	subscriptionClient := loader.DynamicClient.Resource(subscriptionSchema).Namespace("cert-manager-operator")
	_, err = subscriptionClient.Patch(ctx, subName, types.MergePatchType, payload, v1.PatchOptions{})
	return err
}

// waitForCertificateReadiness polls the status of the Certificate object and returns non-nil error
// once the Ready condition is true, otherwise should return a time-out error
func waitForCertificateReadiness(ctx context.Context, certName, namespace string) error {
	return wait.PollImmediate(PollInterval, TestTimeout, func() (bool, error) {
		cert, err := certmanagerClient.CertmanagerV1().Certificates(namespace).Get(ctx, certName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}

		for _, cond := range cert.Status.Conditions {
			if cond.Type == cmv1.CertificateConditionReady {
				return cond.Status == cmmetav1.ConditionTrue, nil
			}
		}
		return false, nil
	})
}

// verifyCertificate loads the tls secret as a X509 certificate and verifies the following
// - certificate secret is non null, i.e. secret contains "tls.crt", "tls.key" keys
// - certificate hasn't expired
// - certificate has subject CN matching provided hostname
func verifyCertificate(ctx context.Context, secretName, namespace, hostname string) error {
	return wait.PollImmediate(PollInterval, TestTimeout, func() (bool, error) {
		secret, err := loader.KubeClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}

		isVerified, err := library.VerifyCertificate(secret, hostname)
		if err != nil {
			return false, err
		}
		return isVerified, nil
	})
}
