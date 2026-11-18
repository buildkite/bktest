@testable import Core
import XCTest

final class TestObserverTests: XCTestCase {
  func testExecutionTagsFlowToTelemetry() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: nil, telemetry: telemetry)

    observer.testCaseWillStart(self)
    observer.setTag(for: self, key: "suite", value: "smoke")
    observer.setTag(for: self, key: "feature", value: "payments")
    observer.setTag(for: self, key: "test.selector.primary", value: "user-supplied-selector")
    observer.testCaseDidFinish(self)

    XCTAssertEqual(recordedTags.value.count, 1)
    XCTAssertEqual(recordedTags.value.first!, [
      "suite": "smoke", "feature": "payments", "test.selector.primary": "user-supplied-selector"
    ])
  }

  func testLateTagIsIgnored() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: nil, telemetry: telemetry)

    observer.testCaseWillStart(self)
    observer.setTag(for: self, key: "before", value: "yes")
    observer.testCaseDidFinish(self)

    observer.setTag(for: self, key: "after", value: "should-not-appear")

    XCTAssertEqual(recordedTags.value.count, 1)
    XCTAssertEqual(recordedTags.value.first!!["before"], "yes")
    XCTAssertNil(recordedTags.value.first!!["after"])
  }

  func testOmitsBundleOutsideBundleExecution() {
    let recordedTests = LockIsolated([TestState]())
    let telemetry = TelemetryClient(
      start: { test in
        recordedTests.withValue { $0.append(test) }
        return test.id
      },
      annotate: { _, _ in },
      finish: { _, _, _ in },
      flush: {}
    )
    let observer = TestObserver(logger: nil, telemetry: telemetry)
    let bundle = Bundle(for: type(of: self))

    // An observer registered after bundle start must not guess the bundle.
    observer.testCaseWillStart(self)
    observer.testCaseDidFinish(self)
    observer.testBundleWillStart(bundle)
    observer.testCaseWillStart(self)
    observer.testCaseDidFinish(self)
    observer.testBundleDidFinish(bundle)
    observer.testCaseWillStart(self)
    observer.testCaseDidFinish(self)

    #if canImport(ObjectiveC)
    XCTAssertEqual(recordedTests.value.map(\.xctestBundleName), [nil, "BuildkiteTestCollectorPackageTests", nil])
    XCTAssertEqual(
      recordedTests.value.map(\.xctestRuntimeClass),
      Array(repeating: NSStringFromClass(type(of: self)), count: 3)
    )
    #else
    XCTAssertEqual(recordedTests.value.map(\.xctestBundleName), [nil, nil, nil])
    XCTAssertEqual(recordedTests.value.map(\.xctestRuntimeClass), [nil, nil, nil])
    #endif
  }

  private func telemetry(onFinish: @escaping ([String: String]?) -> Void) -> TelemetryClient {
    TelemetryClient(
      start: { $0.id },
      annotate: { _, _ in },
      finish: { _, _, tags in onFinish(tags) },
      flush: {}
    )
  }
}
