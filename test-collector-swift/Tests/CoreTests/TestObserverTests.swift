@testable import Core
import XCTest

final class TestObserverTests: XCTestCase {
  func testExecutionTagsFlowToTelemetry() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: nil, telemetry: telemetry, splitBy: "class")

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

  func testAutomaticClassSelector() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: nil, telemetry: telemetry, splitBy: "class")

    observer.testCaseWillStart(self)
    observer.testCaseDidFinish(self)

    XCTAssertEqual(recordedTags.value.count, 1)
    #if canImport(ObjectiveC)
    XCTAssertEqual(recordedTags.value.first!, [
      "test.selector.primary": "BuildkiteTestCollectorPackageTests/TestObserverTests"
    ])
    #else
    XCTAssertEqual(recordedTags.value.first!, ["test.selector.primary": "CoreTests.TestObserverTests"])
    #endif
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

  #if canImport(ObjectiveC)
  func testNativeSelectorsForInheritedAndObjectiveCMethods() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    for splitBy in ["class", "method"] {
      let observer = TestObserver(logger: nil, telemetry: telemetry, splitBy: splitBy)
      for testCase in [
        SelectorBaseCase(selector: #selector(SelectorBaseCase.exampleMethod)),
        SelectorChildCase(selector: #selector(SelectorBaseCase.exampleMethod)),
        SelectorCustomCase(selector: #selector(SelectorCustomCase.exampleMethod)),
      ] {
        observer.testCaseWillStart(testCase)
        observer.testCaseDidFinish(testCase)
      }
    }
    XCTAssertEqual(recordedTags.value.map { $0?["test.selector.primary"] }, [
      "BuildkiteTestCollectorPackageTests/SelectorBaseCase",
      "BuildkiteTestCollectorPackageTests/SelectorChildCase",
      "BuildkiteTestCollectorPackageTests/CollectorCustomCase",
      "BuildkiteTestCollectorPackageTests/SelectorBaseCase/exampleMethod()",
      "BuildkiteTestCollectorPackageTests/SelectorChildCase/exampleMethod()",
      "BuildkiteTestCollectorPackageTests/CollectorCustomCase/exampleMethod",
    ])
  }
  #endif

  func testInvalidSplitWarnsOnceAndDoesNotOverrideFileTimingFallback() {
    let messages = LockIsolated([String]())
    let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: logger, telemetry: telemetry, splitBy: "clas")

    for _ in 0..<2 {
      observer.testCaseWillStart(self)
      observer.testCaseDidFinish(self)
    }
    logger.waitForLogs()

    XCTAssertEqual(recordedTags.value.count, 2)
    XCTAssertTrue(recordedTags.value.allSatisfy { $0 == nil })
    XCTAssertEqual(messages.value, [
      "[BuildkiteTestCollector] warning: Invalid BUILDKITE_TEST_ENGINE_XCTEST_SPLIT_BY value \"clas\"; expected class or method. Using file-based timing history."
    ])
  }

  func testUnsetSplitDoesNotOverrideFileTimingFallback() {
    for splitBy: String? in [nil, ""] {
      let messages = LockIsolated([String]())
      let logger = Logger(printer: { message in messages.withValue { $0.append(message) } })
      let recordedTags = LockIsolated([[String: String]?]())
      let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
      let observer = TestObserver(logger: logger, telemetry: telemetry, splitBy: splitBy)

      observer.testCaseWillStart(self)
      observer.testCaseDidFinish(self)
      logger.waitForLogs()

      XCTAssertEqual(recordedTags.value.count, 1)
      XCTAssertNil(recordedTags.value[0])
      XCTAssertTrue(messages.value.isEmpty)
    }
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

#if canImport(ObjectiveC)
class SelectorBaseCase: XCTestCase {
  @objc func exampleMethod() {}
}
final class SelectorChildCase: SelectorBaseCase {}
@objc(CollectorCustomCase) final class SelectorCustomCase: XCTestCase {
  @objc func exampleMethod() {}
}
#endif
