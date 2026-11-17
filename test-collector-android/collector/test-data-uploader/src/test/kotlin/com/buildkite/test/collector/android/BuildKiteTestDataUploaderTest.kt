package com.buildkite.test.collector.android

import com.buildkite.test.collector.android.model.RunEnvironment
import com.buildkite.test.collector.android.model.TestData
import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.model.TestHistory
import com.buildkite.test.collector.android.model.TestOutcome
import com.buildkite.test.collector.android.model.TestUploadResponse
import com.buildkite.test.collector.android.network.api.TestUploaderApi
import com.buildkite.test.collector.android.network.api.TestUploaderApiFactory
import com.buildkite.test.collector.android.util.logger.Logger
import org.junit.Assert.assertEquals
import org.junit.Test
import org.mockito.Mockito.mock
import org.mockito.Mockito.`when`
import retrofit2.Call
import retrofit2.Response

class BuildKiteTestDataUploaderTest {
    @Test
    fun uploadsEveryExecutionInBoundedChunksWithRunMetadata() {
        val requests = mutableListOf<TestData>()
        val api = object : TestUploaderApi {
            override fun uploadTestData(testData: TestData): Call<TestUploadResponse> {
                requests.add(testData)
                @Suppress("UNCHECKED_CAST")
                val call = mock(Call::class.java) as Call<TestUploadResponse>
                `when`(call.execute()).thenReturn(Response.success(null))
                return call
            }
        }
        val factory = object : TestUploaderApiFactory {
            override fun create(testSuiteApiToken: String) = api
        }
        val run = RunEnvironment()
        val tags = mapOf("language.name" to "kotlin")
        val uploader = BuildKiteTestDataUploader("test-token", run, tags, Logger(), factory)
        val results = (1..10_003).map { index ->
            TestDetails(
                id = "execution-$index",
                scope = "ExampleTest",
                name = "test$index",
                location = null,
                fileName = null,
                result = TestOutcome.Passed,
                failureReason = null,
                history = TestHistory(0, 1, 1.0)
            )
        }

        uploader.uploadTestData(results)

        assertEquals(listOf(5000, 5000, 3), requests.map { it.data.size })
        assertEquals(results.map { it.id }, requests.flatMap { it.data }.map { it.id })
        assertEquals(listOf(run, run, run), requests.map { it.runEnvironment })
        assertEquals(listOf(tags, tags, tags), requests.map { it.tags })
    }
}
