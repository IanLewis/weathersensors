// Original Spark Community Thread: http://community.spark.io/t/how-to-access-the-core-via-we-browser/9711
// Code adapted from: http://arduino.cc/en/Tutorial/WebServer

/* Includes ------------------------------------------------------------------*/  
#include "application.h"
#include "PietteTech_DHT.h"
#include "HttpClient.h"

const String AGGRE_HOST = String("192.168.1.11");
const int AGGRE_PORT = 8000;

const int READ_LED = D0;
const int DHTPIN = D1;
const int PORT = 5000;
const String VERSION = String("1.0");
String deviceName = String("");


void dht_wrapper(); // must be declared before the lib initialization

// Initialize the DHT22 sensor.
PietteTech_DHT dht(DHTPIN, DHT22, dht_wrapper);

// This wrapper is in charge of calling
// mus be defined like this for the lib work
void dht_wrapper() {
    dht.isrCallback();
}

void log(String msg) {
    if (Serial.available()) {
        Serial.println(String("[") + String(Time.now()) + String("] ") + msg);
    }
}

void deviceNameHandler(const char *topic, const char *data) {
    deviceName = String(data);
    log("Got device name: " + deviceName);
}

HttpClient http;

// Headers currently need to be set at init, useful for API keys etc.
http_header_t headers[] = {
    { "Content-Type", "application/x-www-form-urlencoded" },
    { "Accept" , "*/*"},
    { NULL, NULL } // NOTE: Always terminate headers will NULL
};

http_request_t request;
http_response_t response;

int nextRegisterTime = 0;
int registerLoops = 0;

void registerWithAggreHost(int seconds) {
    // Wait for the device name.
    while (deviceName.equals("")) {
        delay(1);
    }

    int now;
    char ipAddress[15]; // holds the ip address

    if (registerLoops > seconds * 100) {
        now = Time.now();
        if (now > nextRegisterTime) {
            IPAddress localIP = WiFi.localIP();
            sprintf(ipAddress, "%d.%d.%d.%d", localIP[0], localIP[1], localIP[2], localIP[3]);

            request.hostname = AGGRE_HOST;
            request.port = AGGRE_PORT;
            request.path = "/api/devices";
            request.body = "name=" + deviceName + "&address=" + ipAddress + "%3A" + PORT;

            log("Registering device: " + deviceName);
            http.post(request, response, headers);
            log("Registered device with status: " + String(response.status));

            nextRegisterTime = now + seconds;
        }
        registerLoops = 0;
    } else {
        registerLoops += 1;
    }
}


int nextPingTime = 0;
int pingLoops = 0;

void debugPing(int seconds, bool conn) {
    int now;

    char ipAddress[15]; // holds the ip address

    if (pingLoops > seconds * 100) {
        now = Time.now();
        if (now > nextPingTime) {
            if (WiFi.ready()) {
                // Once wifi is ready print the status and our IP address.
                IPAddress localIP = WiFi.localIP();
                sprintf(ipAddress, "%d.%d.%d.%d", localIP[0], localIP[1], localIP[2], localIP[3]);
            } else {
                sprintf(ipAddress, "<none>");
            }

            log("PING: DEVICE: " + deviceName + "; VERSION: " + VERSION + "; IP: " + ipAddress + "; PORT: " + String(PORT) + "; CLIENT: " + String(conn));
            nextPingTime = now + seconds;
        }
        pingLoops = 0;
    } else {
        pingLoops += 1;
    }
}

int nextStatusTime = 0;
int statusLoops = 0;

void sendStatus(TCPClient client, int seconds) {
    int now;
    float temp, humidity;
    if (statusLoops > seconds * 100) {
        now = Time.now();
        if (now > nextStatusTime) {
            // Turn on the READ LED.
            digitalWrite(READ_LED, HIGH);

            humidity = dht.readHumidity();
            temp = dht.readTemperature();

            client.println(String("temp:") + String(temp) + String("\thumidity:") + String(humidity));
            nextStatusTime = now + seconds;

            // Delay so that the READ LED stays on
            // for a little longer.
            // This won't have an effect on the loop unless it
            // exceeds the nextStatusTime
            delay(100);
            digitalWrite(READ_LED, LOW);
        }
        statusLoops = 0;
    } else {
        statusLoops += 1;
    }
}

TCPServer server = TCPServer(PORT);
TCPClient client;

void serverMain() {
    // listen for incoming clients
    client = server.available();
    if (client) {
        // an http request ends with a blank line
        if (client.connected()) {
            log(String("Client connected."));
        }
        while (client.connected()) {
            // Send LTSV to client.
            sendStatus(client, 5);
            debugPing(10, true);
    
        }
        log(String("Client disconnected."));
    }

    debugPing(10, false);
    registerWithAggreHost(30);
}


void setup() {
    // start listening for clients
    Serial.begin(115200);

    log("Getting device name...");
    Spark.subscribe("spark/device/name", deviceNameHandler);
    Spark.publish("spark/device/name");

    log("Starting server...");
    server.begin();

    log("Starting DHT22 sensor...");

    pinMode(READ_LED, OUTPUT);
}



// The main loop that gets run forever.
void loop() {
    serverMain();
}
