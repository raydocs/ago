import Foundation
import Testing
@testable import AgoClientCore
#if canImport(Darwin)
import Darwin
#endif

@MainActor @Suite struct PerformanceTests {
    private static let eventCount=5_000
    private static let wallBudget=Duration.seconds(5)
    private static let memoryGrowthBudget=96 * 1_024 * 1_024

    @Test func fiveThousandEventProjectionAndRendererStayWithinFrozenBudget()throws{
        let events=(1...Self.eventCount).map{(UInt64($0),"message.accepted","{\"content\":{\"text\":\"event \($0)\"}}")}
        let raw=Data(ReconnectControllerTests.json(next:UInt64(Self.eventCount),snapshot:UInt64(Self.eventCount),events:events).utf8)
        let memoryBefore=currentAllocatedBytes()
        let clock=ContinuousClock(),start=clock.now
        let projection=try ProjectionDecoder.decode(raw)
        let timeline=EventProjection.timeline(projection.events)
        let elapsed=start.duration(to:clock.now)
        let memoryGrowth=max(0,currentAllocatedBytes()-memoryBefore)
        #expect(projection.events.count==Self.eventCount)
        #expect(timeline.count==Self.eventCount)
        #expect(elapsed < Self.wallBudget,"5,000-event decode/render exceeded frozen 5-second wall budget: \(elapsed)")
        #expect(memoryGrowth < Self.memoryGrowthBudget,"5,000-event decode/render exceeded frozen 96 MiB allocation-growth budget: \(memoryGrowth) bytes")
    }
}

private func currentAllocatedBytes()->Int {
    #if canImport(Darwin)
    var statistics=malloc_statistics_t();malloc_zone_statistics(malloc_default_zone(),&statistics);return statistics.size_in_use
    #else
    return 0
    #endif
}
