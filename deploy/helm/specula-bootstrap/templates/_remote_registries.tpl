{{- /*
CN default remote_registries — mirrors specula.example.yaml k8s chain
(Huawei SWR layout:huawei-ddn → DaoCloud → 1ms). Used when regionProfile=cn
and remoteRegistries is empty.
*/ -}}
{{- define "specula-bootstrap.cnRemoteRegistries" -}}
- host: ghcr.io
  upstreams:
    - name: daocloud
      base_url: https://ghcr.m.daocloud.io
      priority: 1
    - name: 1ms
      base_url: https://ghcr.1ms.run
      priority: 2
- host: quay.io
  upstreams:
    - name: daocloud
      base_url: https://quay.m.daocloud.io
      priority: 1
    - name: 1ms
      base_url: https://quay.1ms.run
      priority: 2
- host: registry.k8s.io
  upstreams:
    - name: huawei-swr
      base_url: https://swr.cn-north-4.myhuaweicloud.com
      layout: huawei-ddn
      priority: 1
    - name: daocloud
      base_url: https://k8s.m.daocloud.io
      priority: 2
    - name: 1ms
      base_url: https://k8s.1ms.run
      priority: 3
- host: gcr.io
  upstreams:
    - name: daocloud
      base_url: https://gcr.m.daocloud.io
      priority: 1
    - name: 1ms
      base_url: https://gcr.1ms.run
      priority: 2
- host: k8s.gcr.io
  upstreams:
    - name: huawei-swr
      base_url: https://swr.cn-north-4.myhuaweicloud.com
      layout: huawei-ddn
      priority: 1
    - name: daocloud
      base_url: https://k8s-gcr.m.daocloud.io
      priority: 2
    - name: 1ms
      base_url: https://k8s.1ms.run
      priority: 3
- host: mcr.microsoft.com
  upstreams:
    - name: daocloud
      base_url: https://mcr.m.daocloud.io
      priority: 1
    - name: 1ms
      base_url: https://mcr.1ms.run
      priority: 2
- host: codeberg.org
{{- end -}}
