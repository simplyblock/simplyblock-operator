# TLS Secrets and ConfigMaps an installation reads

`ControlPlane.spec.source.local.tls` is the only statement of whether the
installation uses TLS, whether callers present client certificates, and which
provider issues the certificates. It is immutable once the object exists, and
`SimplyblockDriver.spec.tls` should be set to the same values.

The names below are fixed. The chart creates the first four rows; on OpenShift,
or wherever the chart's certificate templates are not used, create them in the
operator's namespace before the pods that mount them start.

| Object                                                                  | Kind      | Needed when                         | Created by                                            |
|-------------------------------------------------------------------------|-----------|-------------------------------------|-------------------------------------------------------|
| `simplyblock-certificate-authority` (key `service-ca.crt`)              | ConfigMap | Provider `OpenShift`                | The chart, annotated for OpenShift to inject the CA   |
| `simplyblock-ca-bundle-tls` (key `ca.crt`)                              | Secret    | Provider `cert-manager`             | The chart, as a cert-manager Certificate              |
| `simplyblock-operator-client-tls` (`tls.crt`, `tls.key`, `ca.crt`)      | Secret    | Mutual TLS, provider `cert-manager` | The chart, as a cert-manager Certificate              |
| `<driver>-csi-controller-client-tls` and `<driver>-csi-node-client-tls` | Secret    | Mutual TLS on the driver            | The chart, as cert-manager Certificates               |
| `simplyblock-webappapi-tls`, `simplyblock-foundationdb-tls`             | Secret    | Always with TLS                     | The operator                                          |
| `simplyblock-storage-node-api-tls`, `simplyblock-spdk-proxy-tls`        | Secret    | Always with TLS                     | The operator (cert-manager) or OpenShift's service CA |

`<driver>` is the `SimplyblockDriver`'s name, `simplyblock` for the chart's. The
operator presents a client certificate when the pair is mounted at
`/etc/simplyblock/tls/`, so a deployment without mutual TLS mounts only the CA.
