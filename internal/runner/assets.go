package runner

import _ "embed"

//go:embed svc.sh
var SVCShContent string

//go:embed actions.runner.service.template
var ServiceTemplateContent string

//go:embed container-runner-image/Dockerfile
var containerRunnerDockerfile string

//go:embed container-runner-image/entrypoint.sh
var containerRunnerEntrypoint string