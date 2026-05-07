// FlowScope self-hosted ClickHouse on a single Azure VM.
//
// Provisions:
//   - Standard_D8s_v5 VM (Ubuntu 24.04 LTS, ephemeral OS disk)
//   - Premium SSD v2 data disk (default 1 TiB / 6000 IOPS / 250 MB/s)
//   - Network interface in the supplied subnet, accelerated networking ON
//   - System-assigned managed identity (for Key Vault access)
//   - Cloud-init script that installs ClickHouse, mounts the data disk,
//     applies kernel tuning, and starts the systemd service
//
// See VISION.md §8 for the production architecture context.
//
// Deploy:
//   az deployment group create \
//     --resource-group flowscope-rg \
//     --template-file deploy/infra/clickhouse-vm.bicep \
//     --parameters \
//       vmName=flowscope-clickhouse-01 \
//       subnetId=/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Network/virtualNetworks/<vnet>/subnets/<subnet> \
//       adminPublicKey="$(cat ~/.ssh/id_ed25519.pub)" \
//       clickhousePassword=<strong-secret>

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('VM name (also used as the OS hostname).')
param vmName string

@description('Resource ID of the subnet the VM joins.')
param subnetId string

@description('SSH public key for the admin user `flowscope`.')
param adminPublicKey string

@description('ClickHouse `flowscope` user password. Pull from Key Vault in calling pipeline.')
@secure()
param clickhousePassword string

@description('VM size. D8s_v5 is the documented baseline (8 vCPU / 32 GiB).')
param vmSize string = 'Standard_D8s_v5'

@description('Data disk size in GiB. Premium SSD v2 supports online resize.')
param dataDiskSizeGB int = 1024

@description('Provisioned IOPS for the data disk (Premium SSD v2 only).')
param dataDiskIOPS int = 6000

@description('Provisioned throughput for the data disk in MB/s.')
param dataDiskThroughputMBps int = 250

var dataDiskName = '${vmName}-data'
var nicName = '${vmName}-nic'

// -------- Network interface --------
resource nic 'Microsoft.Network/networkInterfaces@2024-05-01' = {
  name: nicName
  location: location
  properties: {
    enableAcceleratedNetworking: true
    ipConfigurations: [
      {
        name: 'ipconfig1'
        properties: {
          privateIPAllocationMethod: 'Dynamic'
          subnet: { id: subnetId }
        }
      }
    ]
  }
}

// -------- Premium SSD v2 data disk --------
resource dataDisk 'Microsoft.Compute/disks@2024-03-02' = {
  name: dataDiskName
  location: location
  sku: { name: 'PremiumV2_LRS' }
  properties: {
    creationData: { createOption: 'Empty' }
    diskSizeGB: dataDiskSizeGB
    diskIOPSReadWrite: dataDiskIOPS
    diskMBpsReadWrite: dataDiskThroughputMBps
  }
}

// -------- Cloud-init payload --------
// Inline so the deployment is self-contained. Mirrors deploy/infra/cloud-init.yaml;
// keep the two in sync if you edit either.
var cloudInitB64 = base64(replace(replace(loadTextContent('cloud-init.yaml'),
  '__CLICKHOUSE_PASSWORD__', clickhousePassword),
  '__VM_NAME__', vmName))

// -------- Virtual machine --------
resource vm 'Microsoft.Compute/virtualMachines@2024-07-01' = {
  name: vmName
  location: location
  identity: { type: 'SystemAssigned' }
  properties: {
    hardwareProfile: { vmSize: vmSize }
    storageProfile: {
      imageReference: {
        publisher: 'Canonical'
        offer: 'ubuntu-24_04-lts'
        sku: 'server'
        version: 'latest'
      }
      osDisk: {
        createOption: 'FromImage'
        diffDiskSettings: {
          option: 'Local'
          placement: 'CacheDisk'
        }
        caching: 'ReadOnly'
      }
      dataDisks: [
        {
          lun: 0
          name: dataDiskName
          createOption: 'Attach'
          caching: 'None' // Premium SSD v2 forbids host caching
          managedDisk: { id: dataDisk.id }
        }
      ]
    }
    networkProfile: {
      networkInterfaces: [
        { id: nic.id }
      ]
    }
    osProfile: {
      computerName: vmName
      adminUsername: 'flowscope'
      linuxConfiguration: {
        disablePasswordAuthentication: true
        ssh: {
          publicKeys: [
            {
              path: '/home/flowscope/.ssh/authorized_keys'
              keyData: adminPublicKey
            }
          ]
        }
      }
      customData: cloudInitB64
    }
    diagnosticsProfile: {
      bootDiagnostics: { enabled: true }
    }
  }
}

output privateIp string = nic.properties.ipConfigurations[0].properties.privateIPAddress
output principalId string = vm.identity.principalId
output dataDiskResourceId string = dataDisk.id
