package main

// Code was ported over from NOAA's online calculator:
// https://gml.noaa.gov/grad/solcalc/sunrise.html
//
// Most of the equations and functions operate on the Julian century,
// number of centuries since J2000.0
// Only the function to calculate solar noon uses the Julian day.

import (
	"math"
	"time"
)

// factor for degrees to radian conversion
const DEG2RAD = math.Pi / 180

// Calculates Julian day for given date.
// Basically the number of days past since 4713 BCE.
func julianDay(t time.Time) float64 {
	year := t.Year()
	month := int(t.Month())
	if month <= 2 {
		year -= 1
		month += 12
	}
	A := math.Floor(float64(year / 100))
	B := 2 - A + math.Floor(A/4)
	return math.Floor(365.25*float64(year+4716)) +
		math.Floor(30.6001*float64(month+1)) +
		float64(t.Day()) + B - 1524.5
}

// Derive Julian century (number of centuries since J2000.0) given the Julian day
func julianCentury(jd float64) float64 { return (jd - 2451545.) / 36525. }

func meanObliquityOffEcliptic(t float64) float64 {
	seconds := 21.448 - t*(46.8150+t*(0.00059-t*(0.001813)))
	return 23. + (26.+(seconds/60.))/60.
}

func obliquityCorrection(t float64) float64 {
	e0 := meanObliquityOffEcliptic(t)
	omega := 125.04 - 1934.136*t
	e := e0 + 0.00256*math.Cos(DEG2RAD*omega)
	return e
}

func sunGeometricMeanAnomaly(t float64) float64 {
	return 357.52911 + t*(35999.05029-0.0001537*t)
}

func sunEquationOfCenter(t float64) float64 {
	M := DEG2RAD * sunGeometricMeanAnomaly(t)
	C := math.Sin(M)*(1.914602-t*(0.004817+0.000014*t)) +
		math.Sin(2*M)*(0.019993-0.000101*t) +
		math.Sin(3*M)*0.000289
	return C
}

func sunGeometricMeanLong(t float64) float64 {
	return math.Mod(280.46646+t*(36000.76983+0.0003032*t), 360)
}

func sunTrueLong(t float64) float64 {
	return sunGeometricMeanLong(t) + sunEquationOfCenter(t)
}

func sunApparentLong(t float64) float64 {
	omega := 125.04 - 1934.136*t
	return sunTrueLong(t) - 0.00569 - 0.00478*math.Sin(DEG2RAD*omega)
}

func sunEccentricityEarthOrbit(t float64) float64 {
	return 0.016708634 - t*(0.000042037+0.0000001267*t)
}

// Calculate diff between true solar time & mean solar time
func equationOfTime(t float64) float64 {
	epsilon := obliquityCorrection(t)
	l0 := sunGeometricMeanLong(t)
	e := sunEccentricityEarthOrbit(t)
	m := sunGeometricMeanAnomaly(t)

	// convert to radians first
	epsilon *= DEG2RAD
	l0 *= DEG2RAD
	m *= DEG2RAD

	y := math.Tan(epsilon / 2)
	y *= y

	sinM := math.Sin(m)

	Etime := y*math.Sin(2*l0) -
		2*e*sinM +
		4*e*y*sinM*math.Cos(2*l0) -
		0.5*y*y*math.Sin(4*l0) -
		1.25*e*e*math.Sin(2*m)

	return (Etime / DEG2RAD) * 4 // in minutes of time
}

// Calculates UTC solar noon from given Julian day.
// Returns time in minutes
func solarNoonUTC(jd, longitude float64) float64 {
	tnoon := julianCentury(jd + longitude/360)
	eqTime := equationOfTime(tnoon)
	solNoonUTC := 720 + longitude*4 - eqTime // minutes

	// 2nd pass, but with calculated solar noon
	tnoon2 := julianCentury(jd - 0.5 + solNoonUTC/1440)
	eqTime = equationOfTime(tnoon2)
	solNoonUTC = 720 + longitude*4 - eqTime // minutes

	return solNoonUTC
}

// Calculates declination of the Sun, in degrees
func sunDeclination(t float64) float64 {
	e := obliquityCorrection(t)
	lambda := sunApparentLong(t)

	e *= DEG2RAD
	lambda *= DEG2RAD

	return math.Asin(math.Sin(e)*math.Sin(lambda)) / DEG2RAD
}

// Calculates the hour angle of the Sun in degrees.
// Flip the return value sign for sunset
func hourAngle(angle, decl, lat float64) float64 {
	decl *= DEG2RAD
	angle *= DEG2RAD
	lat *= DEG2RAD

	return math.Acos(
		math.Cos(angle)/(math.Cos(lat)*math.Cos(decl))-
			math.Tan(lat)*math.Tan(decl)) / DEG2RAD
}

// Calculate time at which Sun will be at the specified angle.
// Used to calculate sunset/sunrise timings. With an angle of 90.833°, the
// sunset/sunrise time will be returned, depending on the rising parameter.
// Other types of twilight are also possible, like 96° for civil twilight.
// Latitude is +ve in north, -ve in south and longitude is +ve in the west and
// -ve in the east (inverse of normal), all specified in degrees.
func calcTimeAtSunAngle(date time.Time, rising bool, angle, lat, lng float64) time.Time {
	jd := julianDay(date)

	f := func(t float64) float64 {
		eqTime := equationOfTime(t)
		decl := sunDeclination(t)
		angle := hourAngle(angle, decl, lat)
		if !rising {
			angle *= -1
		}

		timeDiff := 4 * (lng - angle) // in minutes
		return 720 + timeDiff - eqTime
	}

	// first pass to approximate sunrise/set using solar noon
	// use the solar noon to find the declination
	noonMin := solarNoonUTC(jd, lng)
	tnoon := julianCentury(jd + noonMin/1440)
	timeUTC := f(tnoon)

	// second pass to include fractional Julian day in gamma
	timeUTC = f(julianCentury(jd + timeUTC/1440))

	return utcMinutesToTime(timeUTC, date)
}

// Converts minutes from UTC into a Time object, relative to specified date.
// The minutes value will be rounded up to the nearest second.
func utcMinutesToTime(minutes float64, date time.Time) time.Time {
	offset := minutes * float64(time.Minute)
	// round up to get seconds resolution
	offset = math.Round(offset/float64(time.Second)) * float64(time.Second)

	// convert the time back into a Time object
	// let it do the UTC conversion for us
	d := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	d = d.Add(time.Duration(offset))
	return d.Local()
}
