package com.buildkite.test.collector.android

import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.tracer.BuildkiteTestObserver
import org.gradle.api.tasks.testing.TestDescriptor
import org.gradle.api.tasks.testing.TestResult
import org.junit.Assert.assertEquals
import org.junit.Test
import org.mockito.Mockito.mock
import org.mockito.Mockito.`when`

class UnitTestListenerTest {
    @Test
    fun uploadsClassSelectorsForEveryMethodWithoutLosingOtherTags() {
        val uploaded = mutableListOf<TestDetails>()
        val uploader = object : TestDataUploader {
            override fun uploadTestData(testCollection: List<TestDetails>) {
                uploaded.addAll(testCollection)
            }
        }
        val observer = BuildkiteTestObserver()
        val listener = UnitTestListener(uploader, observer)
        val result = mock(TestResult::class.java)
        `when`(result.resultType).thenReturn(TestResult.ResultType.SUCCESS)

        for (className in listOf("example.FirstTest", "example.FirstTest", "example.SecondTest")) {
            val descriptor = mock(TestDescriptor::class.java)
            `when`(descriptor.className).thenReturn(className)
            `when`(descriptor.displayName).thenReturn("aMethod")
            listener.beforeTest(descriptor)
            observer.setExecutionTag("feature", "checkout")
            listener.afterTest(descriptor, result)
        }
        listener.afterSuite(mock(TestDescriptor::class.java), result)

        assertEquals(
            listOf("example.FirstTest", "example.FirstTest", "example.SecondTest"),
            uploaded.map { it.tags?.get("test.selector.primary") }
        )
        assertEquals(listOf("checkout", "checkout", "checkout"), uploaded.map { it.tags?.get("feature") })
    }
}
