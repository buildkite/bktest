import XCTest

extension XCTestCase {
  /// Returns the class name of the test.
  static func className(of testCase: XCTestCase) -> String {
    "\(type(of: testCase))"
  }

  /// The native runnable unit, using the concrete class for inherited tests.
  static func selector(of testCase: XCTestCase, splitByMethod: Bool) -> String {
    #if canImport(ObjectiveC)
    let runtimeClass = NSStringFromClass(type(of: testCase))
    let testClass = runtimeClass.split(separator: ".").last!
    let target = Bundle(for: type(of: testCase)).bundleURL.deletingPathExtension().lastPathComponent
    var selector = "\(target)/\(testClass)"
    if splitByMethod {
      // Xcode enumerates Swift methods with (), but custom Objective-C and
      // mangled nested class identifiers use the bare Objective-C selector.
      selector += "/\(self.testName(of: testCase))\(runtimeClass.contains(".") ? "()" : "")"
    }
    #else
    // swift-corelibs-xctest uses SwiftPM's Module.Class[/method] identifiers.
    var selector = String(reflecting: type(of: testCase))
    if splitByMethod { selector += "/\(self.testName(of: testCase))" }
    #endif
    return selector
  }

  /// Returns the name of the test.
  static func testName(of testCase: XCTestCase) -> String {
    #if canImport(ObjectiveC)
    // [XCTestCase testName]
    return String(
      testCase.name
        .split(separator: " ", maxSplits: 1).last!
        .dropLast(testCase.name.last == "]" ? 1 : 0)
    )
    #else
    // XCTestCase.testName
    return String(testCase.name.split(separator: ".").last!)
    #endif
  }
}
