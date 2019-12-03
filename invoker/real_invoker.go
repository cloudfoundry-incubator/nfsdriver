package invoker

import (
	"code.cloudfoundry.org/dockerdriver"
	"code.cloudfoundry.org/goshims/execshim"
	"code.cloudfoundry.org/lager"
	"fmt"
)

type realInvoker struct {
	useExec execshim.Exec
}

func NewRealInvoker() Invoker {
	return NewRealInvokerWithExec(&execshim.ExecShim{})
}

func NewRealInvokerWithExec(useExec execshim.Exec) Invoker {
	return &realInvoker{useExec}
}

func (r *realInvoker) Invoke(env dockerdriver.Env, executable string, cmdArgs []string) ([]byte, error) {
	logger := env.Logger().Session("invoking-command", lager.Data{"executable": executable, "args": cmdArgs})
	logger.Info("start")
	defer logger.Info("end")

	cmdHandle := r.useExec.CommandContext(env.Context(), executable, cmdArgs...)

	output, err := cmdHandle.CombinedOutput()
	if err != nil {
		logger.Error("invocation-failed", err, lager.Data{"output": output, "exe": executable})
		return output, fmt.Errorf("%s - details:\n%s", err.Error(), output)
	}

	return output, nil
}
