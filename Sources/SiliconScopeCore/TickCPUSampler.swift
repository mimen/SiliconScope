//
//  File:      TickCPUSampler.swift
//  Created:   2026-09-12
//  Updated:   2026-09-12
//  Developer: Kennt Kim / Calida Lab
//  Overview:  Whole-machine CPU utilization from host_processor_info ticks, with no IOReport
//             dependency. `CPUSampler` cannot construct on an Intel Mac — it needs the "CPU Stats"
//             IOReport group, which those machines do not publish — so every Intel host reported
//             0% CPU on 0 cores (#56). The tick arithmetic it already used works fine there.
//  Notes:     Same per-core busy/total delta as CPUSampler's usage pass, averaged across all
//             logical CPUs instead of split by perf level: an Intel Mac has one tier, so a cluster
//             split would be a distinction without a difference. Stateful for the same reason
//             CPUSampler is — the first call has no previous reading and returns 0 rather than
//             inventing a number from a single absolute sample.
//
import Foundation

public final class TickCPUSampler {
    private var previousBusy: [UInt64] = []
    private var previousTotal: [UInt64] = []

    public init() {}

    /// Mean busy fraction across all logical CPUs, 0...1. Zero on the first call (no delta yet).
    public func sampleUsage() -> Double {
        var cpuCount: natural_t = 0
        var info: processor_info_array_t?
        var infoCount: mach_msg_type_number_t = 0
        guard host_processor_info(mach_host_self(), PROCESSOR_CPU_LOAD_INFO,
                                  &cpuCount, &info, &infoCount) == KERN_SUCCESS,
              let info else { return 0 }
        defer {
            let size = vm_size_t(infoCount) * vm_size_t(MemoryLayout<integer_t>.stride)
            vm_deallocate(mach_task_self_, vm_address_t(bitPattern: info), size)
        }

        let count = Int(cpuCount)
        let states = Int(CPU_STATE_MAX)
        var busy = [UInt64](repeating: 0, count: count)
        var total = [UInt64](repeating: 0, count: count)
        for i in 0..<count {
            let user = UInt64(info[i * states + Int(CPU_STATE_USER)])
            let system = UInt64(info[i * states + Int(CPU_STATE_SYSTEM)])
            let nice = UInt64(info[i * states + Int(CPU_STATE_NICE)])
            let idle = UInt64(info[i * states + Int(CPU_STATE_IDLE)])
            busy[i] = user + system + nice
            total[i] = user + system + nice + idle
        }
        defer { previousBusy = busy; previousTotal = total }

        guard previousBusy.count == count, count > 0 else { return 0 }

        var sum = 0.0
        for i in 0..<count {
            let deltaBusy = Double(busy[i] >= previousBusy[i] ? busy[i] - previousBusy[i] : 0)
            let deltaTotal = Double(total[i] >= previousTotal[i] ? total[i] - previousTotal[i] : 0)
            sum += deltaTotal > 0 ? deltaBusy / deltaTotal : 0
        }
        return sum / Double(count)
    }
}
