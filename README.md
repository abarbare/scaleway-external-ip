# scaleway-external-ip (this is a POC, all hell can break loose, use at your own risk)

This project aims to bring some sort of IP failover mechanism, over Scaleway's new routed IP system. 

## Description

scaleway-external-ip brings a new CRD: `ScwExternalIP`. This resource, with the help of Scaleway Routed IPs, will allow you to have a failover IP that always is pointing to a healthy node of the cluster.

In order to make it work, you'll need a `ClusterIP` service, with the `.spec.externalIPs` set to the Scaleway Routed IPs (v4&v6, see limitations) you want to use for this service, like this: 

```yaml
apiVersion: v1
kind: Service
metadata:
  name: myapp
  labels:
    app: myapp
spec:
  ports:
  - port: 8080
    targetPort: 8080
  selector:
    app: myapp
  externalIPs:
    - 1.2.3.4
    - dead::beef::1
```

In this case, the service `myapp` will be exposed on the public endpoints `1.2.3.4:8080` and `dead::beef::1:8080`. 
However, the IPs needs to be attached to a node in the cluster, and the addresses added to the interface. 

No worries, you just need to create the following `ScwExternalIP`:

```yaml
apiVersion: ptrk.io/v1alpha1
kind: ScwExternalIP
metadata:
  name: myapp
spec:
  service: myapp # name of the targeted service, in the same namespace
  # supports the nodeSelector, when choosing a node to attach. 
  # the controller is already adding a selector on the IP's zone
  #nodeSelector:
  # TODO: ideas to add reverse, whitelist (might be done with a Cilium network policy though), ...
```

Once it's created (and if the agents and controller are running of course!), it will attach the different IPs to a node matching the constraints.
The agent will add the IP address on the Instance's interface on all nodes matchings the constraints, for a quick failover.
Once a node is not ready, the IP is detached, and re-attached to another node matching the constraints.

TODO: add a healtcheck mechanism for faster failure discovery
TODO: or add a new CRD to manage IP addresses on the nodes, and check every X for last heartbeat

## Limitations

- No real IPv6 support right now, as the Kapsule cluster can't be dual stack. The IP will be confiugred on the host, but Cilium will reject connections because it's not ipv6 enable.
- "Slow" failover, as it waits for the node to be not ready
- Needs [IP Mobility](https://www.scaleway.com/en/blog/ip-mobility-removing-nat/) to be enable to work

## Getting Started

### Warning

Currently, Kubernetes Kapsule does not support IP mobility. There is a [workaround to manually migrate your nodes](https://www.scaleway.com/en/docs/compute/instances/api-cli/using-routed-ips/) which can be used.
However, all your nodes needs to be migrated (with a reboot), and every new node added automatically will need to be migrated too.

As usual, please don't do this on your production :) 

### Setup

You’ll need a Scaleway Kapsule Kubernetes cluster to run against (could work with a selfhosted cluster on Scaleway instances, though I haven't tested it) running Cilium as CNI.

Install the controller and the agent with:
```yaml
kubectl create -k https://github.com/Sh4d1/scaleway-external-ip/config/default
```

Create and enter your Scaleway credentials with:
```yaml
kubectl create -f https://raw.githubusercontent.com/Sh4d1/scaleway-external-ip/main/secret.yaml --edit --namespace scaleway-external-ip-system
```

You are now ready to create Routed IPs, add them to `externalIPs` in a service, and create a `ScwExternalIP` targeting this service! Yay!  

## License

Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

