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

  func testAutomaticClassSelector() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    let observer = TestObserver(logger: nil, telemetry: telemetry)

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

    // Tag set after finish should be ignored
    observer.setTag(for: self, key: "after", value: "should-not-appear")

    XCTAssertEqual(recordedTags.value.count, 1)
    XCTAssertEqual(recordedTags.value.first!!["before"], "yes")
    XCTAssertNil(recordedTags.value.first!!["after"])
  }

  #if canImport(ObjectiveC)
  func testNativeSelectorsForInheritedAndObjectiveCMethods() {
    let recordedTags = LockIsolated([[String: String]?]())
    let telemetry = self.telemetry { tags in recordedTags.withValue { $0.append(tags) } }
    for splitByMethod in [false, true] {
      let observer = TestObserver(logger: nil, telemetry: telemetry, splitByMethod: splitByMethod)
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
