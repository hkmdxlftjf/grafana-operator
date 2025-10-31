package grafana

import (
	"context"
	"fmt"
	"slices"

	"github.com/grafana/grafana-operator/v5/api/v1beta1"
	"github.com/grafana/grafana-operator/v5/controllers/model"
	"github.com/grafana/grafana-operator/v5/controllers/reconcilers"
	ingress "github.com/openshift/api/operatoringress"
	routev1 "github.com/openshift/api/route/v1"
	v1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	v2 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	RouteKind = "Route"
)

type HttpRouteReconciler struct {
	client client.Client
}

func NewHttpRouteReconciler(client client.Client) reconcilers.OperatorGrafanaReconciler {
	return &HttpRouteReconciler{
		client: client,
	}
}

func (r *HttpRouteReconciler) Reconcile(ctx context.Context, cr *v1beta1.Grafana, vars *v1beta1.OperatorReconcileVars, scheme *runtime.Scheme) (v1beta1.OperatorStageStatus, error) {
	log := logf.FromContext(ctx).WithName("IngressReconciler")

	log.Info("reconciling http route")

	return r.reconcileIngress(ctx, cr, vars, scheme)
}

func (r *HttpRouteReconciler) reconcileIngress(ctx context.Context, cr *v1beta1.Grafana, _ *v1beta1.OperatorReconcileVars, scheme *runtime.Scheme) (v1beta1.OperatorStageStatus, error) {
	if cr.Spec.HttpRoute == nil {
		return v1beta1.OperatorStageResultSuccess, nil
	}

	httpRoute := model.GetGrafanaHttpRoute(cr, scheme)

	_, err := controllerutil.CreateOrUpdate(ctx, r.client, ingress, func() error {
		httpRoute.Spec = getIngressSpec(cr, scheme)

		err := v1beta1.Merge(ingress, cr.Spec.Ingress)
		if err != nil {
			setInvalidMergeCondition(cr, "Ingress", err)
			return err
		}

		removeInvalidMergeCondition(cr, "Ingress")

		err = controllerutil.SetControllerReference(cr, ingress, scheme)
		if err != nil {
			return err
		}

		model.SetInheritedLabels(ingress, cr.Labels)

		return nil
	})
	if err != nil {
		return v1beta1.OperatorStageResultFailed, err
	}

	// try to assign the admin url
	if cr.PreferIngress() {
		adminURL := r.getHttpRouteAdminURL(ctx, ingress)

		if len(ingress.Status.LoadBalancer.Ingress) == 0 {
			return v1beta1.OperatorStageResultInProgress, fmt.Errorf("ingress is not ready yet")
		}

		if adminURL == "" {
			return v1beta1.OperatorStageResultFailed, fmt.Errorf("ingress spec is incomplete")
		}

		cr.Status.AdminURL = adminURL
	}

	return v1beta1.OperatorStageResultSuccess, nil
}

// getIngressAdminURL returns the first valid URL (Host field is set) from the ingress spec
func (r *IngressReconciler) getHttpRouteAdminURL(ctx context.Context, httpRoute *v2.HTTPRoute) string {
	log := logf.FromContext(ctx)
	if httpRoute == nil {
		return ""
	}

	protocol := "http"

	var (
		hostname string
		adminURL string
	)

	// An ingress rule might not have the field Host specified, better not to consider such rules
	if len(httpRoute.Spec.Hostnames) > 0 {
		hostname = string(httpRoute.Spec.Hostnames[0])
	}
	gw := &v2.Gateway{}
	if len(httpRoute.Spec.ParentRefs) > 0 {
		pr := httpRoute.Spec.ParentRefs[0]

		gwnn := types.NamespacedName{
			Namespace: string(*pr.Namespace),
			Name:      string(pr.Name),
		}
		err := r.client.Get(ctx, gwnn, gw)
		if err != nil {
			log.Error(err, "error synchronizing grafana statuses")
			return ""
		}
	}

	if hostname == "" {
		loadBalanceIP := ""

		for _, address := range gw.Status.Addresses {
			if address.Value != "" {
				loadBalanceIP = address.Value
				break
			}
		}
		if loadBalanceIP != "" {
			hostname = loadBalanceIP
		}
	}

	// If we can find the target host in any of the IngressTLS, then we should use https protocol
	for _, listener := range gw.Spec.Listeners {
		//listener.AllowedRoutes.Kinds
	}

	// if all fails, try to get access through the load balancer
	if hostname == "" {
		loadBalancerIP := ""

		for _, lb := range ingress.Status.LoadBalancer.Ingress {
			if lb.Hostname != "" {
				hostname = lb.Hostname
				break
			}

			if lb.IP != "" {
				loadBalancerIP = lb.IP
			}
		}

		if hostname == "" && loadBalancerIP != "" {
			hostname = loadBalancerIP
		}
	}

	// adminUrl should not be empty only in case hostname is found, otherwise we'll have broken URLs like "http://"
	if hostname != "" {
		adminURL = fmt.Sprintf("%v://%v", protocol, hostname)
	}

	return adminURL
}

func GetHttpRouteTargetPort(cr *v1beta1.Grafana) intstr.IntOrString {
	return intstr.FromInt(GetGrafanaPort(cr))
}

func getHttpRouteSpec(cr *v1beta1.Grafana, scheme *runtime.Scheme) v2.HTTPRouteSpec {
	service := model.GetGrafanaService(cr, scheme)

	port := GetHttpRouteTargetPort(cr)
	serviceName := v2.ObjectName(service.GetName())
	serviceNamespace := v2.Namespace(service.GetNamespace())
	servicePort := v2.PortNumber(port.IntValue())

	var assignedPort v1.ServiceBackendPort
	if port.IntVal > 0 {
		assignedPort.Number = port.IntVal
	}

	if port.StrVal != "" {
		assignedPort.Name = port.StrVal
	}

	pathType := v2.PathMatchPathPrefix
	path := "/"

	return v2.HTTPRouteSpec{
		Rules: []v2.HTTPRouteRule{{
			BackendRefs: []v2.HTTPBackendRef{
				{
					BackendRef: v2.BackendRef{
						BackendObjectReference: v2.BackendObjectReference{
							Name:      serviceName,
							Namespace: &serviceNamespace,
							Port:      &servicePort,
						},
					},
				},
			},
			Matches: []v2.HTTPRouteMatch{
				{
					Path: &v2.HTTPPathMatch{
						Type:  &pathType,
						Value: &path,
					},
				},
			},
		}},
	}
}
