package com.buildkite.test.collector.android

import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.model.TestFailureExpanded
import com.buildkite.test.collector.android.model.TestHistory
import com.buildkite.test.collector.android.tracer.TestObserver
import org.gradle.api.tasks.testing.TestDescriptor
import org.gradle.api.tasks.testing.TestListener
import org.gradle.api.tasks.testing.TestResult

internal class UnitTestListener(
    private val testUploader: TestDataUploader,
    private val testObserver: TestObserver
) : TestListener {
    private val testCollection: MutableList<TestDetails> = mutableListOf()

    override fun beforeSuite(suite: TestDescriptor) = Unit

    override fun afterSuite(suite: TestDescriptor, result: TestResult) {
        if (suite.className == null && suite.parent == null) {
            testUploader.uploadTestData(testCollection = testCollection)
        }
    }

    override fun beforeTest(testDescriptor: TestDescriptor) = Unit

    override fun afterTest(testDescriptor: TestDescriptor, result: TestResult) {
        when (result.resultType) {
            TestResult.ResultType.SUCCESS -> {
                testObserver.recordSuccess()
            }

            TestResult.ResultType.FAILURE -> {
                val failureReason = result.exception.toString()
                val failureDetails = result.exceptions.map { exception ->
                    TestFailureExpanded(
                        expanded = listOf("${exception.message}:${exception.cause}"),
                        backtrace = exception.stackTraceToString().split("\n")
                            .map { it.trim() }
                    )
                }
                testObserver.recordFailure(
                    reason = failureReason,
                    details = failureDetails
                )
            }

            TestResult.ResultType.SKIPPED -> {
                testObserver.recordSkipped()
            }

            null -> {
                testObserver.recordFailure(
                    reason = "TestResult type was unexpectedly null, indicating an error in the test framework."
                )
            }
        }

        addTestDetailsToCollection(test = testDescriptor, result = result)
    }

    private fun addTestDetailsToCollection(test: TestDescriptor, result: TestResult) {
        val tags = testObserver.executionTags.toMutableMap()
        // Gradle selects the whole runnable ancestor, including suite members.
        generateSequence(test) { it.parent }.mapNotNull { it.className }.lastOrNull()?.let { selector ->
            tags["test.selector.primary"] = selector
        }
        val testHistory = TestHistory(
            startAt = result.startTime / 1000,
            endAt = result.endTime / 1000,
            duration = (result.endTime - result.startTime) / 1000.0
        )

        val testDetails = TestDetails(
            scope = test.className,
            name = test.displayName,
            location = test.className,
            fileName = null,
            result = testObserver.outcome,
            failureReason = testObserver.failureReason,
            failureExpanded = testObserver.failureDetails,
            tags = tags.ifEmpty { null },
            history = testHistory
        )

        testCollection.add(testDetails)
        testObserver.reset()
    }
}
