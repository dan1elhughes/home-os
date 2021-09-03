#!/usr/bin/env python3
import Adafruit_DHT
import time
import os
import sys
import socket
import systemd.daemon
from influxdb import InfluxDBClient

serverIp = os.environ.get('INFLUX_SERVER_IP', '192.168.1.128')
print('Env: serverIp ', serverIp)
serverPort = os.environ.get('INFLUX_SERVER_PORT', 8086)
print('Env: serverPort ', serverPort)
serverDatabase = os.environ.get('INFLUX_SERVER_DATABASE', 'external')
print('Env: serverDatabase ', serverDatabase)

client = InfluxDBClient(host=serverIp, port=serverPort,
                        database=serverDatabase)

location = os.environ.get('LOCATION', '')
if location is '':
    sys.exit('Missing location')


hostname = socket.gethostname()

# Notify systemd that we're done loading.
systemd.daemon.notify('READY=1')


DHT_SENSOR = Adafruit_DHT.DHT22
DHT_PIN = 2

while True:
    try:
        tags = {}
        metrics = {}

        tags['hostname'] = hostname
        tags['location'] = location

        metrics['tags'] = tags
        metrics['measurement'] = 'ambient_temperature'

        humidity, temperature = Adafruit_DHT.read_retry(DHT_SENSOR, DHT_PIN)
        fields = {}

        if humidity is not None:
            fields['humidity'] = humidity
        if temperature is not None:
            fields['temperature'] = temperature

        metrics['fields'] = fields

        client.write_points([metrics])
        print(metrics)
    except RuntimeError as error:
        print(error.args[0])

    time.sleep(10)
