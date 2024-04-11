# tfzstackit (fork from tfz53 - previously knows as bzfttr53rdutil)
A conversion utility for creating [Terraform](https://terraform.io) resource definitions for STACKIT DNS from BIND zonefiles.

## Installation
Download the [latest release](https://github.com/Slm0n87/tfzstackit/releases/latest).

## Usage
`tfzstackit -domain <domain-name> [flags] > stackit-domain.tf`

## Flags
| Name       | Description                                        | Default         |
|------------|----------------------------------------------------|-----------------|
| -domain    | Name of domain. Required.                          |                 |
| -zone-file | Path to zone file. Optional.                       | `<domain>.zone` |
| -exclude   | Record types to ignore, comma-separated. Optional. | `SOA,NS`        |


## Building
If you want to build from source, you will first need the Go tools. Instructions for installation are available from the [documentation](https://golang.org/doc/install#install).

Once that is done, run 

```bash
go get github.com/Slm0n87/tfzstackit
cd $GOPATH/src/github.com/Slm0n87/tfzstackit
go build
```

You should now have a finished binary.

This project uses `dep` to manage external dependencies. See the [Github repo](https://github.com/golang/dep) for more information.
