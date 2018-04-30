// This file was automatically generated by lister-gen

package v1

import (
	v1 "github.com/openshift/origin/pkg/quota/apis/quota/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// AppliedClusterResourceQuotaLister helps list AppliedClusterResourceQuotas.
type AppliedClusterResourceQuotaLister interface {
	// List lists all AppliedClusterResourceQuotas in the indexer.
	List(selector labels.Selector) (ret []*v1.AppliedClusterResourceQuota, err error)
	// AppliedClusterResourceQuotas returns an object that can list and get AppliedClusterResourceQuotas.
	AppliedClusterResourceQuotas(namespace string) AppliedClusterResourceQuotaNamespaceLister
	AppliedClusterResourceQuotaListerExpansion
}

// appliedClusterResourceQuotaLister implements the AppliedClusterResourceQuotaLister interface.
type appliedClusterResourceQuotaLister struct {
	indexer cache.Indexer
}

// NewAppliedClusterResourceQuotaLister returns a new AppliedClusterResourceQuotaLister.
func NewAppliedClusterResourceQuotaLister(indexer cache.Indexer) AppliedClusterResourceQuotaLister {
	return &appliedClusterResourceQuotaLister{indexer: indexer}
}

// List lists all AppliedClusterResourceQuotas in the indexer.
func (s *appliedClusterResourceQuotaLister) List(selector labels.Selector) (ret []*v1.AppliedClusterResourceQuota, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1.AppliedClusterResourceQuota))
	})
	return ret, err
}

// AppliedClusterResourceQuotas returns an object that can list and get AppliedClusterResourceQuotas.
func (s *appliedClusterResourceQuotaLister) AppliedClusterResourceQuotas(namespace string) AppliedClusterResourceQuotaNamespaceLister {
	return appliedClusterResourceQuotaNamespaceLister{indexer: s.indexer, namespace: namespace}
}

// AppliedClusterResourceQuotaNamespaceLister helps list and get AppliedClusterResourceQuotas.
type AppliedClusterResourceQuotaNamespaceLister interface {
	// List lists all AppliedClusterResourceQuotas in the indexer for a given namespace.
	List(selector labels.Selector) (ret []*v1.AppliedClusterResourceQuota, err error)
	// Get retrieves the AppliedClusterResourceQuota from the indexer for a given namespace and name.
	Get(name string) (*v1.AppliedClusterResourceQuota, error)
	AppliedClusterResourceQuotaNamespaceListerExpansion
}

// appliedClusterResourceQuotaNamespaceLister implements the AppliedClusterResourceQuotaNamespaceLister
// interface.
type appliedClusterResourceQuotaNamespaceLister struct {
	indexer   cache.Indexer
	namespace string
}

// List lists all AppliedClusterResourceQuotas in the indexer for a given namespace.
func (s appliedClusterResourceQuotaNamespaceLister) List(selector labels.Selector) (ret []*v1.AppliedClusterResourceQuota, err error) {
	err = cache.ListAllByNamespace(s.indexer, s.namespace, selector, func(m interface{}) {
		ret = append(ret, m.(*v1.AppliedClusterResourceQuota))
	})
	return ret, err
}

// Get retrieves the AppliedClusterResourceQuota from the indexer for a given namespace and name.
func (s appliedClusterResourceQuotaNamespaceLister) Get(name string) (*v1.AppliedClusterResourceQuota, error) {
	obj, exists, err := s.indexer.GetByKey(s.namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1.Resource("appliedclusterresourcequota"), name)
	}
	return obj.(*v1.AppliedClusterResourceQuota), nil
}