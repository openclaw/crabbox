//go:build darwin && cgo

package serve

/*
#include <mach/mach.h>
static int serve_cpu_ticks(unsigned long long *total, unsigned long long *idle) {
    host_cpu_load_info_data_t info;
    mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
    mach_port_t host = mach_host_self();
    kern_return_t result = host_statistics(host, HOST_CPU_LOAD_INFO, (host_info_t)&info, &count);
    mach_port_deallocate(mach_task_self(), host);
    if (result != KERN_SUCCESS) return 0;
    *total = (unsigned long long)info.cpu_ticks[CPU_STATE_USER] + info.cpu_ticks[CPU_STATE_SYSTEM]
           + info.cpu_ticks[CPU_STATE_NICE] + info.cpu_ticks[CPU_STATE_IDLE];
    *idle = info.cpu_ticks[CPU_STATE_IDLE];
    return 1;
}
*/
import "C"

import "context"

func (h *hostSampler) darwinCPU(_ context.Context) *float64 {
	var total, idle C.ulonglong
	if C.serve_cpu_ticks(&total, &idle) == 0 {
		return nil
	}
	return h.cpuDelta(uint64(total), uint64(idle))
}
