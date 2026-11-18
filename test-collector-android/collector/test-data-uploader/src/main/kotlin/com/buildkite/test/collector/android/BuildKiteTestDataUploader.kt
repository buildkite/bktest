package com.buildkite.test.collector.android

import com.buildkite.test.collector.android.model.RunEnvironment
import com.buildkite.test.collector.android.model.TestData
import com.buildkite.test.collector.android.model.TestDetails
import com.buildkite.test.collector.android.model.TestUploadResponse
import com.buildkite.test.collector.android.network.api.DefaultTestUploaderApiFactory
import com.buildkite.test.collector.android.network.api.TestUploaderApiFactory
import com.buildkite.test.collector.android.util.CollectorUtils
import com.buildkite.test.collector.android.util.logger.Logger
import retrofit2.Response

/**
 * Uploads test data to Buildkite Test Analytics.
 */
class BuildKiteTestDataUploader internal constructor(
    private val testSuiteApiToken: String,
    private val runEnvironment: RunEnvironment,
    private val uploadTags: Map<String, String>,
    private val logger: Logger,
    private val testUploaderApiFactory: TestUploaderApiFactory
) : TestDataUploader {
    /**
     * Creates an uploader for a test suite.
     *
     * @param testSuiteApiToken The test suite API token for authentication.
     * @param runEnvironment The test run environment configuration.
     * @param uploadTags Optional upload-level tags that apply to all test executions.
     * @param logger The logger for upload messages.
     */
    constructor(
        testSuiteApiToken: String,
        runEnvironment: RunEnvironment,
        uploadTags: Map<String, String> = emptyMap(),
        logger: Logger = Logger()
    ) : this(testSuiteApiToken, runEnvironment, uploadTags, logger, DefaultTestUploaderApiFactory())

    init {
        logger.debug { "TestDataUploader initialized with test analytics API token." }
        if (runEnvironment.ci != null) {
            logger.debug { "CI system detected: ${runEnvironment.ci}" }
        } else {
            logger.debug { "No CI system detected." }
        }
    }

    /**
     * Uploads test results in batches of up to [CollectorUtils.Uploader.TEST_DATA_UPLOAD_LIMIT].
     *
     * @param testCollection The test results to upload.
     */
    override fun uploadTestData(testCollection: List<TestDetails>) {
        testCollection.chunked(CollectorUtils.Uploader.TEST_DATA_UPLOAD_LIMIT).forEach { chunk ->
            uploadTestData(prepareTestData(chunk))
        }
    }

    private fun prepareTestData(testCollection: List<TestDetails>): TestData {
        return TestData(
            format = "json",
            runEnvironment = runEnvironment,
            tags = uploadTags.ifEmpty { null },
            data = testCollection
        )
    }

    private fun uploadTestData(testData: TestData) {
        logger.debug { "Uploading test analytics data." }

        runCatching {
            val testUploaderApi = testUploaderApiFactory.create(testSuiteApiToken)
            val uploadTestDataApiCall = testUploaderApi.uploadTestData(testData = testData)

            uploadTestDataApiCall.execute()
        }.onSuccess { testUploadResponse ->
            logApiResponse(testUploadResponse = testUploadResponse)
        }.onFailure { throwable ->
            logger.error { "Error uploading test analytics data: ${throwable.message}." }
        }
    }

    private fun logApiResponse(testUploadResponse: Response<TestUploadResponse>) {
        if (testUploadResponse.isSuccessful) {
            logger.info { "Test analytics data successfully uploaded. URL: ${testUploadResponse.body()?.runUrl}" }
        } else {
            logger.error {
                "Error uploading test analytics data. HTTP error code: ${testUploadResponse.code()}. Ensure the test suite API token is correct and properly configured."
            }
            logger.error { "Error response details: ${testUploadResponse.errorBody()?.string()}" }
        }
    }
}
